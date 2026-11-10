package controlpanel

import (
	"fmt"
	"slices"
)

var ErrNicknameReservedBound = fmt.Errorf("previously connected credential: %w", ErrNicknameTaken)
var ErrNicknameReservedUnbound = fmt.Errorf("unused credential: %w", ErrNicknameTaken)

// tokenReservesName includes both sides of an interrupted rename until the
// durable registration tells us which write committed.
func tokenReservesName(t SessionToken, name string) bool {
	return !t.Revoked && t.ExpiredAt.IsZero() && t.Kind != tokenProject && (t.Nickname == name || t.PendingNickname == name)
}

func (s *Store) nicknameTokenLocked(project, id, identity string, generation <-chan struct{}) (Project, int, error) {
	if s.closed || s.expireLocked() != nil || identity == "" {
		return Project{}, 0, ErrInvalid
	}
	select {
	case <-generation:
		return Project{}, 0, ErrInvalid
	default:
	}
	p, ok := s.data.Projects[project]
	if !ok || !p.Enabled || p.Deleting || p.Mode != "token" {
		return Project{}, 0, ErrInvalid
	}
	for i, t := range p.Tokens {
		if t.ID == id && t.BoundIdentity == identity && s.sessionTokenEligibleLocked(p, t) {
			return p, i, nil
		}
	}
	return Project{}, 0, ErrInvalid
}

// Called with the stored registration name under the broker mutation lock.
// It decides whether to complete or roll back an interrupted policy journal.
func (s *Store) checkNickname(project, id, identity, name string, generation <-chan struct{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, i, err := s.nicknameTokenLocked(project, id, identity, generation)
	if err != nil {
		return err
	}
	t := p.Tokens[i]
	if name != t.Nickname && (t.PendingNickname == "" || name != t.PendingNickname) {
		return ErrInvalid
	}
	if t.PendingNickname == "" {
		return nil
	}
	p.Tokens = append([]SessionToken(nil), p.Tokens...)
	p.Tokens[i].Nickname, p.Tokens[i].PendingNickname = name, ""
	p.AutoBindings = slices.Clone(p.AutoBindings)
	for j, b := range p.AutoBindings {
		if b.GrantID == id && b.PendingRevision != 0 {
			if name == t.PendingNickname {
				p.AutoBindings[j].ExpectedRevision = b.PendingRevision
			}
			p.AutoBindings[j].PendingRevision = 0
		}
	}
	return s.commitProject(p)
}

func (s *Store) renameNickname(project, id, identity, oldName, newName string, generation <-chan struct{}, apply func() error) error {
	if !validNickname(newName) || apply == nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, i, err := s.nicknameTokenLocked(project, id, identity, generation)
	if err != nil {
		return err
	}
	if p.Tokens[i].Nickname != oldName || p.Tokens[i].PendingNickname != "" {
		return ErrInvalid
	}
	for j, t := range p.Tokens {
		if j != i && tokenReservesName(t, newName) {
			if t.FirstConnectedAt.IsZero() {
				return ErrNicknameReservedUnbound
			}
			return ErrNicknameReservedBound
		}
	}
	p.Tokens = append([]SessionToken(nil), p.Tokens...)
	p.Tokens[i].PendingNickname = newName
	p.AutoBindings = slices.Clone(p.AutoBindings)
	for j, b := range p.AutoBindings {
		if b.GrantID == id && b.State != "confirmed" {
			p.AutoBindings[j].PendingRevision = b.ExpectedRevision + 1
		}
	}
	if err := s.commitProject(p); err != nil {
		return err
	}
	applyErr := apply()
	// commitProject published the previous slice; copy before changing it.
	p.Tokens = append([]SessionToken(nil), p.Tokens...)
	p.Tokens[i].PendingNickname = ""
	if applyErr == nil {
		p.Tokens[i].Nickname = newName
	}
	p.AutoBindings = slices.Clone(p.AutoBindings)
	for j, b := range p.AutoBindings {
		if b.GrantID == id && b.PendingRevision != 0 {
			if applyErr == nil {
				p.AutoBindings[j].ExpectedRevision = b.PendingRevision
			}
			p.AutoBindings[j].PendingRevision = 0
		}
	}
	if err := s.commitProject(p); err != nil {
		return err
	}
	return applyErr
}
