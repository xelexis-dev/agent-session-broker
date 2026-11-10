package controlpanel

import (
	"crypto/subtle"
	"encoding/hex"
	"maps"
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"
)

func validProfile(displayName, email string) bool {
	if !utf8.ValidString(displayName) || utf8.RuneCountInString(displayName) > 100 || !utf8.ValidString(email) || len(email) > 254 {
		return false
	}
	for _, value := range []string{displayName, email} {
		for _, r := range value {
			if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
				return false
			}
		}
	}
	if email != "" {
		address, err := mail.ParseAddress(email)
		if err != nil || address.Name != "" || address.Address != email {
			return false
		}
	}
	return true
}

// UpdateProfile never accepts a target account or authorization fields. The
// caller supplies its authenticated actor; ordinary metadata keeps logins valid.
func (s *Store) UpdateProfile(actor, displayName, email string, version ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.activeUserLocked(actor)
	if !ok {
		return ErrForbidden
	}
	if err := checkSettingsVersion(version, profileSettingsVersion(u.User)); err != nil {
		return err
	}
	if !validProfile(displayName, email) {
		return ErrInvalid
	}
	displayName = strings.TrimSpace(displayName)
	u.DisplayName = displayName
	u.Email = email
	candidate := s.data
	candidate.Users = maps.Clone(s.data.Users)
	candidate.Users[actor] = u
	return s.commit(candidate)
}

// ChangeOwnPassword verifies the current credential and publishes the new hash
// and login revision under one lock, including the fixed admin's legacy hash.
func (s *Store) ChangeOwnPassword(actor, currentPassword, newPassword, confirmation string) error {
	if len(currentPassword) > 1024 || len(newPassword) < 12 || len(newPassword) > 1024 || newPassword != confirmation {
		return ErrInvalid
	}
	s.hashSlot <- struct{}{}
	defer func() { <-s.hashSlot }()
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.activeUserLocked(actor)
	if !ok {
		return ErrForbidden
	}
	salt, _ := hex.DecodeString(u.Password.Salt)
	expected, _ := hex.DecodeString(u.Password.Hash)
	actual := derivePasswordKey([]byte(currentPassword), salt)
	if subtle.ConstantTimeCompare(expected, actual) != 1 {
		return ErrInvalid
	}
	verifier, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	u.Password = verifier
	u.Revision++
	candidate := s.data
	candidate.Users = maps.Clone(s.data.Users)
	candidate.Users[actor] = u
	if actor == "admin" {
		candidate.Admin = &verifier
	}
	return s.commit(candidate)
}
