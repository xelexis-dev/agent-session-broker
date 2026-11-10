package controlpanel

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"
)

// Unlike encoding/json's bool, null is not a valid policy decision.
type policyBool bool

func (b *policyBool) UnmarshalJSON(raw []byte) error {
	switch string(raw) {
	case "true":
		*b = true
	case "false":
		*b = false
	default:
		return ErrInvalid
	}
	return nil
}

type ScopeIssuanceChange struct {
	Actor string     `json:"actor"`
	At    time.Time  `json:"at"`
	Old   policyBool `json:"old"`
	New   policyBool `json:"new"`
}

func (e *ScopeIssuanceChange) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 4 {
		return ErrInvalid
	}
	for _, key := range []string{"actor", "at", "old", "new"} {
		if _, ok := fields[key]; !ok {
			return ErrInvalid
		}
	}
	type plain ScopeIssuanceChange
	var parsed plain
	if json.Unmarshal(raw, &parsed) != nil {
		return ErrInvalid
	}
	*e = ScopeIssuanceChange(parsed)
	return nil
}

const maxScopePolicyChanges = 1024

func validScopePolicy(st state) bool {
	if (st.Version < scopeIntroductionVersion && (st.ScopeIssuanceReady || len(st.ScopeIssuanceChanges) > 0)) || len(st.ScopeIssuanceChanges) > maxScopePolicyChanges {
		return false
	}
	previous := policyBool(false)
	var at time.Time
	for _, e := range st.ScopeIssuanceChanges {
		if e.Actor == "" || len(e.Actor) > 128 || e.At.IsZero() || e.At.Before(at) || e.Old != previous || e.Old == e.New {
			return false
		}
		previous, at = e.New, e.At
	}
	// Audit metadata is optional on load; every change through the setter is audited.
	return len(st.ScopeIssuanceChanges) == 0 || previous == st.ScopeIssuanceReady
}
func (s *Store) scopeIssuancePolicy() (bool, *ScopeIssuanceChange) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.data.ScopeIssuanceChanges) == 0 {
		return bool(s.data.ScopeIssuanceReady), nil
	}
	last := s.data.ScopeIssuanceChanges[len(s.data.ScopeIssuanceChanges)-1]
	return bool(s.data.ScopeIssuanceReady), &last
}
func (s *Store) SetScopeIssuanceReady(actor string, enabled bool, version ...string) error {
	return s.setScopeIssuanceReadyGuarded(actor, enabled, nil, version...)
}
func (s *Store) setScopeIssuanceReadyGuarded(actor string, enabled bool, guard commitGuard, version ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := guard.check(); err != nil {
		return err
	}
	if s.closed || s.data.Admin == nil {
		return ErrStorage
	}
	if !s.isAdminLocked(actor) {
		return ErrForbidden
	}
	if err := checkSettingsVersion(version, policySettingsVersion("scope-issuance", bool(s.data.ScopeIssuanceReady))); err != nil {
		return err
	}
	if bool(s.data.ScopeIssuanceReady) == enabled {
		return nil
	}
	if len(s.data.ScopeIssuanceChanges) >= maxScopePolicyChanges {
		return ErrStorage
	}
	next := s.data
	next.ScopeIssuanceReady = policyBool(enabled)
	next.ScopeIssuanceChanges = append(slices.Clone(s.data.ScopeIssuanceChanges), ScopeIssuanceChange{actor, s.now().UTC(), s.data.ScopeIssuanceReady, policyBool(enabled)})
	return s.commit(next)
}
func (p *panel) scopeIssuanceMutate(w http.ResponseWriter, r *http.Request, session loginSession) {
	user, ok := p.store.User(session.userID)
	if !ok || !user.Enabled || user.Role != "admin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	for key, values := range r.PostForm {
		if key == "settings_version" {
			continue
		}
		if (key != "csrf" && key != "enabled") || len(values) != 1 {
			http.Error(w, "invalid form", 400)
			return
		}
	}
	if err := checkSettingsVersion(r.PostForm["settings_version"], policySettingsVersion("scope-issuance", p.store.scopedIssuanceReady())); err != nil {
		p.show(w, r, session, "admin-settings", "", http.StatusConflict, settingsConflictMessage, "")
		return
	}
	raw := r.PostForm.Get("enabled")
	if raw != "true" && raw != "false" {
		http.Error(w, "invalid form", 400)
		return
	}
	if err := p.store.SetScopeIssuanceReady(session.userID, raw == "true", r.PostForm["settings_version"]...); err != nil {
		if errors.Is(err, errSettingsConflict) {
			p.show(w, r, session, "admin-settings", "", http.StatusConflict, settingsConflictMessage, "")
			return
		}
		p.show(w, r, session, "admin-settings", "", 400, "토큰 발급 설정을 저장하지 못했습니다.", "")
		return
	}
	http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
}

var _ json.Unmarshaler = (*policyBool)(nil)
