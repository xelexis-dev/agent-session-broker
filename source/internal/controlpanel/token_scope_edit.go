package controlpanel

import (
	"encoding/hex"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

const tokenScopesAuditKind = "token-scopes-changed"

var errTokenScopeHistory = errors.New("terminal credential scopes would be empty")

// TokenScopeEdit contains only non-secret editor data; the version is not authorization.
type TokenScopeEdit struct {
	Project         string   `json:"project"`
	ParentID        string   `json:"id"`
	Label           string   `json:"label"`
	OwnerID         string   `json:"ownerId"`
	Scopes          []string `json:"scopes"`
	Version         string   `json:"settings_version"`
	ChangedChildren int      `json:"changedChildren"`
	UsableChildren  int      `json:"usableChildren"`
	Auto            bool     `json:"auto"`
	MaxAutoBindings int      `json:"maxAutoBindings"`
	LifetimeDays    int      `json:"lifetimeDays"`
	CanWiden        bool     `json:"canWiden"`
}
type TokenScopeAudit struct {
	ActorID         string   `json:"actorId"`
	Label           string   `json:"label"`
	Before          []string `json:"before"`
	After           []string `json:"after"`
	ChangedChildren int      `json:"changedChildren"`
}

func tokenScopeVersion(project, generation string, t SessionToken) string {
	return settingsVersion("token-scopes", project, append([]string{generation, t.ID}, t.Scopes...)...)
}
func (s *Store) tokenScopeTargetLocked(actor, project, id string) (Project, int, error) {
	p, ok := s.data.Projects[project]
	role := s.projectRoleLocked(actor, project)
	if s.closed {
		return Project{}, 0, ErrStorage
	}
	if !ok || !p.Enabled || p.Deleting || p.Mode != "token" || roleRank(role) < 2 {
		return Project{}, 0, ErrForbidden
	}
	for i, t := range p.Tokens {
		if t.ID == id && t.Kind == tokenProject && tokenActive(t, s.now()) && s.tokenOwnerEligibleLocked(p, t.OwnerID) && (role == "owner" || t.OwnerID == actor) {
			return p, i, nil
		}
	}
	return Project{}, 0, ErrForbidden
}
func (s *Store) tokenScopeViewLocked(actor string, p Project, i int) TokenScopeEdit {
	t := p.Tokens[i]
	v := TokenScopeEdit{Project: p.Name, ParentID: t.ID, Label: t.Label, OwnerID: t.OwnerID, Scopes: slices.Clone(t.Scopes), Version: tokenScopeVersion(p.Name, p.ProjectGenerationID, t), CanWiden: s.projectRoleLocked(actor, p.Name) == "owner" && bool(s.data.ScopeIssuanceReady)}
	v.Auto = access.HasScope(t.Scopes, access.SessionRegisterAuto)
	v.MaxAutoBindings = t.MaxAutoBindings
	if !t.ExpiresAt.IsZero() {
		v.LifetimeDays = int(t.ExpiresAt.Sub(t.IssuedAt) / (24 * time.Hour))
	}
	for _, child := range p.Tokens {
		if child.ParentID == t.ID && s.sessionTokenEligibleLocked(p, child) {
			v.UsableChildren++
		}
	}
	return v
}
func (s *Store) TokenScopeEditFor(actor, project, id string) (TokenScopeEdit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, i, err := s.tokenScopeTargetLocked(actor, project, id)
	if err != nil {
		return TokenScopeEdit{}, err
	}
	return s.tokenScopeViewLocked(actor, p, i), nil
}
func scopeIntersection(old, next []string) []string {
	out := make([]string, 0, len(old))
	for _, scope := range old {
		if access.HasScope(next, scope) {
			out = append(out, scope)
		}
	}
	return out
}
func (s *Store) EditTokenScopesFor(actor, project, id string, scopes, version []string) (TokenScopeEdit, error) {
	return s.editTokenScopesForGuarded(actor, project, id, scopes, version, nil)
}
func (s *Store) editTokenScopesForGuarded(actor, project, id string, scopes, version []string, guard commitGuard) (TokenScopeEdit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := guard.check(); err != nil {
		return TokenScopeEdit{}, err
	}
	return s.prepareTokenScopeEditLocked(actor, project, id, scopes, version, true)
}
func (s *Store) PreviewTokenScopesFor(actor, project, id string, scopes, version []string) (TokenScopeEdit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prepareTokenScopeEditLocked(actor, project, id, scopes, version, false)
}
func (s *Store) prepareTokenScopeEditLocked(actor, project, id string, scopes, version []string, apply bool) (TokenScopeEdit, error) {
	p, i, err := s.tokenScopeTargetLocked(actor, project, id)
	if err != nil {
		return TokenScopeEdit{}, err
	}
	view := s.tokenScopeViewLocked(actor, p, i)
	if len(version) != 1 || len(version[0]) != 64 || version[0] != strings.ToLower(version[0]) {
		return view, ErrInvalid
	}
	if _, err := hex.DecodeString(version[0]); err != nil {
		return view, ErrInvalid
	}
	if version[0] != view.Version {
		return view, errSettingsConflict
	}
	next := slices.Clone(scopes)
	slices.Sort(next)
	if !access.IssuableScopes(next) || !access.HasScope(next, access.SessionRegister) || access.HasScope(next, access.SessionRegisterAuto) {
		return view, ErrInvalid
	}
	parent := p.Tokens[i]
	if access.HasScope(parent.Scopes, access.SessionRegisterAuto) {
		next = append(next, access.SessionRegisterAuto)
		slices.Sort(next)
	}
	if !access.ScopesSubset(next, parent.Scopes) && !view.CanWiden {
		return view, ErrForbidden
	}
	if slices.Equal(next, parent.Scopes) {
		return view, nil
	}
	delegated := access.DelegatedScopes(next)
	p.Tokens = slices.Clone(p.Tokens)
	p.Devices = slices.Clone(p.Devices)
	p.Tokens[i].Scopes = next
	var changed []string
	for j, t := range p.Tokens {
		if t.ParentID != parent.ID {
			continue
		}
		usable := s.sessionTokenEligibleLocked(s.data.Projects[project], t)
		updated := scopeIntersection(t.Scopes, delegated)
		if usable {
			updated = slices.Clone(delegated)
		}
		if len(updated) == 0 {
			return view, errTokenScopeHistory
		}
		if usable && !slices.Equal(t.Scopes, updated) {
			changed = append(changed, t.ID)
		}
		p.Tokens[j].Scopes = updated
	}
	for j, d := range p.Devices {
		if d.TokenID != parent.ID {
			continue
		}
		updated := scopeIntersection(d.Scopes, delegated)
		if d.Status == "pending" && s.now().Before(d.ExpiresAt) {
			updated = slices.Clone(delegated)
		}
		if len(updated) == 0 {
			return view, errTokenScopeHistory
		}
		p.Devices[j].Scopes = updated
	}
	candidate := s.data
	candidate.Projects = maps.Clone(s.data.Projects)
	candidate.Projects[project] = p
	// Preflight before any commit, audit publication or stream interruption.
	if !validState(candidate) || !validScopeState(candidate) || !validScopePolicy(candidate) || !validAutoState(candidate) {
		return view, ErrStorage
	}
	view.ChangedChildren = len(changed)
	if !apply {
		return view, nil
	}
	event := AuditEvent{Version: 1, Project: project, ProjectGenerationID: p.ProjectGenerationID, ParentID: parent.ID, OwnerID: parent.OwnerID, Kind: tokenScopesAuditKind, At: s.now().UTC(), TokenScopes: &TokenScopeAudit{ActorID: actor, Label: parent.Label, Before: slices.Clone(parent.Scopes), After: slices.Clone(next), ChangedChildren: len(changed)}}
	if err := s.commitAudited(candidate, []AuditEvent{event}); err != nil {
		return view, err
	}
	for _, id := range changed {
		key := project + "/" + id
		if ch := s.tokenChanges[key]; ch != nil {
			close(ch)
			delete(s.tokenChanges, key)
		}
	}
	view = s.tokenScopeViewLocked(actor, s.data.Projects[project], i)
	view.ChangedChildren = len(changed)
	return view, nil
}
func validTokenScopeAudit(e AuditEvent) bool {
	a := e.TokenScopes
	return a != nil && e.Version == 1 && e.Kind == tokenScopesAuditKind && projectName.MatchString(e.Project) && registrationNickname.MatchString(e.ProjectGenerationID) && e.OwnerID != "" && len(e.ParentID) == 52 && e.GrantID == "" && e.Binding == "" && e.Reason == "" && e.Handover == nil && e.Management == nil && !e.At.IsZero() && a.ActorID != "" && len(a.Label) <= 128 && utf8.ValidString(a.Label) && !strings.ContainsAny(a.Label, "\x00\r\n") && access.ValidScopes(a.Before) && access.ValidScopes(a.After) && access.HasScope(a.Before, access.SessionRegister) && access.HasScope(a.After, access.SessionRegister) && access.HasScope(a.Before, access.SessionRegisterAuto) == access.HasScope(a.After, access.SessionRegisterAuto) && !slices.Equal(a.Before, a.After) && a.ChangedChildren >= 0
}
