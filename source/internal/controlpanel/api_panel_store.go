package controlpanel

import (
	"encoding/json"
	"time"
)

type adminAPISetting struct {
	V       int       `json:"v"`
	Enabled bool      `json:"enabled"`
	Actor   string    `json:"actor,omitempty"`
	At      time.Time `json:"at,omitempty"`
	Old     bool      `json:"old"`
	New     bool      `json:"new"`
}

func (s *Store) adminAPIPolicySnapshot() (bool, string, *ScopeIssuanceChange) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	enabled := !s.closed && s.api.enabled
	version := policySettingsVersion("admin-api", enabled)
	if s.api.lastSetting == nil {
		return enabled, version, nil
	}
	last := s.api.lastSetting
	return enabled, version, &ScopeIssuanceChange{Actor: last.Actor, At: last.At, Old: policyBool(last.Old), New: policyBool(last.New)}
}
func (s *Store) adminAPISettingBytesLocked() []byte {
	setting := adminAPISetting{V: 1, Enabled: s.api.enabled}
	if s.api.lastSetting != nil {
		setting = *s.api.lastSetting
	}
	setting.Enabled = s.api.enabled
	raw, _ := json.Marshal(setting)
	return raw
}
func (s *Store) RevokeOwnUserToken(actor, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.activeUserLocked(actor); !ok {
		return ErrForbidden
	}
	return s.revokeUserTokensLocked(actor, id, false)
}
func (s *Store) UserTokensFor(actor, user string) ([]UserTokenView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.isAdminLocked(actor) {
		return nil, ErrForbidden
	}
	if _, ok := s.data.Users[user]; !ok {
		return nil, ErrInvalid
	}
	return s.userTokenViewsLocked(user), nil
}
func (s *Store) RevokeUserTokensFor(actor, user, id string, all bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isAdminLocked(actor) {
		return ErrForbidden
	}
	if _, ok := s.data.Users[user]; !ok {
		return ErrInvalid
	}
	return s.revokeUserTokensLocked(user, id, all, actor)
}
