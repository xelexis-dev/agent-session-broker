package controlpanel

import (
	"crypto/rand"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

const connectSettingKey = "settings/connect"
const connectSettingDiagnostic = "connect_disabled"
const maxConnectAudit = 4096
const maxConnectAuditPerParent = 256
const maxConnectAuditPerUser = 512

var (
	errConnectDisabled  = deviceError("connect_disabled")
	errConnectForbidden = deviceError("connect_forbidden")
	errConnectCapacity  = deviceError("connect_capacity")
)

type connectProject struct {
	Name       string `json:"name"`
	Generation string `json:"generation"`
}
type connectOptions struct {
	AdmissionMode string           `json:"admission_mode"`
	AutoProjects  []connectProject `json:"auto_projects"`
	MCPScopes     []string         `json:"mcp_scopes"`
	MaxSessions   int              `json:"max_sessions"`
}
type connectSetting struct {
	V       int         `json:"v"`
	Enabled *policyBool `json:"enabled"`
	Actor   string      `json:"actor,omitempty"`
	At      time.Time   `json:"at,omitempty"`
}
type connectAudit struct {
	V                 int       `json:"v"`
	At                time.Time `json:"at"`
	Kind              string    `json:"kind"`
	Reason            string    `json:"reason"`
	ParentKind        string    `json:"parent_kind"`
	ParentID          string    `json:"parent_id"`
	UserID            string    `json:"user_id"`
	Project           string    `json:"project"`
	ProjectGeneration string    `json:"project_generation"`
	GrantID           string    `json:"grant_id"`
	ActorKind         string    `json:"actor_kind"`
	ActorID           string    `json:"actor_id"`
}
type connectState struct {
	enabled    bool
	diagnostic string
	audits     map[string]connectAudit
}

func isConnectToken(t userAPIToken) bool {
	return len(t.Scopes) == 1 && t.Scopes[0] == "connect" && t.Connect != nil
}
func validConnectOptions(c *connectOptions) bool {
	if c == nil || (c.AdmissionMode != "manual" && c.AdmissionMode != "auto") || c.MaxSessions < 1 || c.MaxSessions > maxAutoBindingLimit || !access.IssuableScopes(c.MCPScopes) || !access.HasScope(c.MCPScopes, access.SessionRegister) || len(c.AutoProjects) > 128 {
		return false
	}
	if c.AdmissionMode == "manual" && (len(c.AutoProjects) != 0 || access.HasScope(c.MCPScopes, access.SessionRegisterAuto)) {
		return false
	}
	if c.AdmissionMode == "auto" && !access.HasScope(c.MCPScopes, access.SessionRegisterAuto) {
		return false
	}
	seen := map[string]bool{}
	for _, p := range c.AutoProjects {
		if !projectName.MatchString(p.Name) || !registrationNickname.MatchString(p.Generation) || seen[p.Name] {
			return false
		}
		seen[p.Name] = true
	}
	return true
}
func validConnectAudit(e connectAudit) bool {
	if e.V != 1 || e.At.IsZero() || !slices.Contains([]string{"token-issued", "token-revoked", "grant-issued", "grant-revoked", "project-selected", "switch-changed", "auto-project-removed"}, e.Kind) || !slices.Contains([]string{"explicit", "expired", "user-changed", "membership-lost", "project-deleted", "wrong-endpoint", "enabled", "disabled", "approved", "auto"}, e.Reason) {
		return false
	}
	if e.ActorKind == "user" {
		if !usernamePattern.MatchString(e.ActorID) {
			return false
		}
	} else if e.ActorKind != "system" || e.ActorID != "" {
		return false
	}
	if e.Kind == "switch-changed" {
		return e.ParentKind == "" && e.ParentID == "" && e.UserID == "" && e.Project == "" && e.ProjectGeneration == "" && e.GrantID == ""
	}
	if e.ParentKind != "user-connect" || !apiTokenIDPattern.MatchString(e.ParentID) || !usernamePattern.MatchString(e.UserID) {
		return false
	}
	if e.GrantID != "" && !apiTokenIDPattern.MatchString(e.GrantID) {
		return false
	}
	if e.Project == "" {
		return e.ProjectGeneration == "" && e.GrantID == "" && (e.Kind == "token-issued" || e.Kind == "token-revoked")
	}
	return projectName.MatchString(e.Project) && registrationNickname.MatchString(e.ProjectGeneration) && ((e.Kind != "grant-issued" && e.Kind != "grant-revoked") || e.GrantID != "")
}

func (s *Store) loadConnect(records map[string][]byte) {
	s.connect = connectState{diagnostic: connectSettingDiagnostic, audits: map[string]connectAudit{}}
	var setting connectSetting
	if raw := records[connectSettingKey]; len(raw) <= 4096 && strictAPIBytes(raw, &setting) == nil && setting.V == 1 && setting.Enabled != nil {
		s.connect.enabled = bool(*setting.Enabled)
		if s.connect.enabled {
			s.connect.diagnostic = ""
		}
	}
	for key, raw := range records {
		if strings.HasPrefix(key, "connect-audit/") {
			var e connectAudit
			if len(raw) <= 4096 && strictAPIBytes(raw, &e) == nil && validConnectAudit(e) {
				s.connect.audits[key] = e
			}
		}
	}
}
func connectEvent(t userAPIToken, project Project, grant, kind, reason, actor string) connectAudit {
	e := connectAudit{V: 1, Kind: kind, Reason: reason, ParentKind: "user-connect", ParentID: t.ID, UserID: t.User, Project: project.Name, ProjectGeneration: project.ProjectGenerationID, GrantID: grant, ActorKind: "system"}
	if actor != "" {
		e.ActorKind = "user"
		e.ActorID = actor
	}
	return e
}
func connectTokenLive(st state, t userAPIToken, now time.Time) bool {
	u, ok := st.Users[t.User]
	return ok && u.Enabled && u.Revision == t.Rev && now.Before(t.ExpiresAt) && isConnectToken(t)
}
func (s *Store) connectParentLocked(p Project, id string, auto bool) (userAPIToken, error) {
	t, ok := s.api.tokens[id]
	if !ok || !connectTokenLive(s.data, t, s.now()) {
		return t, errConnectForbidden
	}
	for _, other := range p.Tokens {
		if other.ID == id && other.Kind == tokenProject {
			return t, errConnectForbidden
		}
	}
	if s.closed || !s.connect.enabled {
		return t, errConnectDisabled
	}
	if !p.Enabled || p.Deleting || p.Mode != "token" || roleRank(p.Members[t.User]) < roleRank("developer") {
		return t, errConnectForbidden
	}
	if auto && (t.Connect.AdmissionMode != "auto" || p.Members[t.User] != "owner" || !slices.Contains(t.Connect.AutoProjects, connectProject{p.Name, p.ProjectGenerationID})) {
		return t, errDirectForbidden
	}
	return t, nil
}
func connectSessionParent(t userAPIToken) SessionToken {
	return SessionToken{ID: t.ID, OwnerID: t.User, ScopeVersion: access.ScopeVersion, Scopes: slices.Clone(t.Connect.MCPScopes), AdmissionMode: t.Connect.AdmissionMode, ExpiresAt: t.ExpiresAt, MaxAutoBindings: t.Connect.MaxSessions}
}

// authenticateConnect deliberately has no admin API or transport-policy dependency.
func (s *Store) authenticateConnect(raw string) (userAPIToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return userAPIToken{}, ErrStorage
	}
	if !s.connect.enabled {
		return userAPIToken{}, errConnectDisabled
	}
	if !strings.HasPrefix(raw, "asb_u_") || len(raw) != 58 {
		return userAPIToken{}, errAPIUnauthorized
	}
	hash := digest(raw)
	for _, t := range s.api.tokens {
		if equal(t.Hash, hash) && connectTokenLive(s.data, t, s.now()) {
			return cloneConnectToken(t), nil
		}
	}
	return userAPIToken{}, errAPIUnauthorized
}
func cloneConnectToken(t userAPIToken) userAPIToken {
	t.Scopes = slices.Clone(t.Scopes)
	if t.Connect != nil {
		c := *t.Connect
		c.MCPScopes = slices.Clone(c.MCPScopes)
		c.AutoProjects = slices.Clone(c.AutoProjects)
		t.Connect = &c
	}
	return t
}

// Internal issuance only. No panel or HTTP handler calls this in MR1.
func (s *Store) issueConnectToken(user string, seconds int64, c connectOptions) (string, userAPIToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", userAPIToken{}, ErrStorage
	}
	if !s.connect.enabled {
		return "", userAPIToken{}, errConnectDisabled
	}
	if err := s.sweepAPILocked(); err != nil {
		return "", userAPIToken{}, err
	}
	u, ok := s.activeUserLocked(user)
	if !ok {
		return "", userAPIToken{}, ErrForbidden
	}
	if c.AdmissionMode == "" {
		c.AdmissionMode = "manual"
	}
	if c.MaxSessions == 0 {
		c.MaxSessions = 32
		if c.AdmissionMode == "auto" {
			c.MaxSessions = 8
		}
	}
	if c.MCPScopes == nil {
		c.MCPScopes = access.LegacyV1Scopes()
		if c.AdmissionMode == "auto" {
			c.MCPScopes = append(c.MCPScopes, access.SessionRegisterAuto)
		}
	}
	c.MCPScopes = slices.Clone(c.MCPScopes)
	slices.Sort(c.MCPScopes)
	c.AutoProjects = slices.Clone(c.AutoProjects)
	if c.AutoProjects == nil {
		c.AutoProjects = []connectProject{}
	}
	if !validConnectOptions(&c) {
		return "", userAPIToken{}, ErrInvalid
	}
	for _, p := range c.AutoProjects {
		project, exists := s.data.Projects[p.Name]
		if !exists || !project.Enabled || project.Deleting || project.ProjectGenerationID != p.Generation || project.Members[user] != "owner" {
			return "", userAPIToken{}, ErrForbidden
		}
	}
	days := 180
	maximum := 365
	if c.AdmissionMode == "auto" {
		days = 30
		maximum = 90
	}
	if seconds == 0 {
		seconds = int64(days * 24 * 3600)
	}
	if seconds < 300 || seconds > int64(maximum*24*3600) {
		return "", userAPIToken{}, ErrInvalid
	}
	count := 0
	for _, t := range s.api.tokens {
		if t.User == user {
			count++
		}
	}
	if count >= 32 || len(s.api.tokens) >= 1024 || len(s.api.rows) >= 2048 {
		return "", userAPIToken{}, errAPITokenCap
	}
	raw := "asb_u_" + rand.Text() + rand.Text()
	now := s.now().UTC()
	t := userAPIToken{V: 1, ID: rand.Text() + rand.Text(), Hash: digest(raw), User: user, Rev: u.Revision, Scopes: []string{"connect"}, Source: "panel", CreatedAt: now, ExpiresAt: now.Add(time.Duration(seconds) * time.Second), Connect: &c}
	tokens := maps.Clone(s.api.tokens)
	tokens[t.ID] = t
	if err := s.commitConnect(s.data, nil, nil, nil, tokens, []connectAudit{connectEvent(t, Project{}, "", "token-issued", "explicit", user)}, nil); err != nil {
		return "", userAPIToken{}, err
	}
	return raw, cloneConnectToken(t), nil
}
func (s *Store) setConnectPolicy(actor string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isAdminLocked(actor) {
		return ErrForbidden
	}
	value := policyBool(enabled)
	setting := connectSetting{V: 1, Enabled: &value, Actor: actor, At: s.now().UTC()}
	raw, _ := json.Marshal(setting)
	reason := "disabled"
	if enabled {
		reason = "enabled"
	}
	e := connectAudit{V: 1, Kind: "switch-changed", Reason: reason, ActorKind: "user", ActorID: actor}
	if err := s.commitConnect(s.data, nil, nil, map[string][]byte{connectSettingKey: raw}, nil, []connectAudit{e}, nil); err != nil {
		return err
	}
	s.connect.enabled = enabled
	s.connect.diagnostic = connectSettingDiagnostic
	if enabled {
		s.connect.diagnostic = ""
	}
	s.notifyConnectWatchersLocked()
	return nil
}
func (s *Store) notifyConnectWatchersLocked() {
	for project, p := range s.data.Projects {
		for _, t := range p.Tokens {
			if _, ok := s.api.tokens[t.ParentID]; ok && !s.sessionTokenEligibleLocked(p, t) {
				key := project + "/" + t.ID
				if ch := s.tokenChanges[key]; ch != nil {
					close(ch)
					delete(s.tokenChanges, key)
				}
			}
		}
	}
}

// Grants and reservations share one parent budget across projects, deduplicated by grant ID.
func connectCount(st state, parent string, now time.Time) int {
	ids := map[string]bool{}
	for _, p := range st.Projects {
		for _, t := range p.Tokens {
			if t.ParentID == parent && tokenActive(t, now) {
				ids[t.ID] = true
			}
		}
		for _, b := range p.AutoBindings {
			if b.ParentID != parent {
				continue
			}
			active := true
			for _, t := range p.Tokens {
				if t.ID == b.GrantID && !tokenActive(t, now) {
					active = false
				}
			}
			if active && (b.State != "issued" || now.Before(b.ExpiresAt)) {
				ids[b.GrantID] = true
			}
		}
	}
	return len(ids)
}
func (s *Store) sweepConnectLocked() error {
	for _, t := range s.api.tokens {
		if isConnectToken(t) && !connectTokenLive(s.data, t, s.now()) {
			return s.commit(s.data)
		}
	}
	return nil
}

func (s *Store) revokeConnectUserTokensLocked(user, id string, all bool, actors ...string) error {
	tokens := maps.Clone(s.api.tokens)
	deletes := []string{}
	for tokenID, t := range tokens {
		if t.User == user && (all || tokenID == id) {
			delete(tokens, tokenID)
			deletes = append(deletes, t.key())
		}
	}
	if all {
		rows, err := s.listRecords()
		if err != nil {
			return ErrStorage
		}
		for key := range rows {
			if strings.HasPrefix(key, "user-tokens/"+user+"/") {
				deletes = append(deletes, key)
			}
		}
	}
	actor := user
	if len(actors) == 1 {
		actor = actors[0]
	}
	return s.commitConnect(s.data, nil, &apiAuditEvent{Action: "token_revoke", Actor: actor, Target: user, Result: "success"}, nil, tokens, nil, deletes)
}

// connect=false preserves the project-token-only public API until MR2 exposes
// dedicated HTTP paths. Tests exercise the same internals with connect=true.
func (s *Store) admissionParentLocked(project, raw string, connect, auto bool) (Project, SessionToken, error) {
	if !connect {
		return s.projectTokenLocked(project, raw)
	}
	p := s.data.Projects[project]
	if !strings.HasPrefix(raw, "asb_u_") || len(raw) != 58 {
		return p, SessionToken{}, errAPIUnauthorized
	}
	hash := digest(raw)
	for id, t := range s.api.tokens {
		if equal(t.Hash, hash) && isConnectToken(t) {
			parent, err := s.connectParentLocked(p, id, auto)
			if err != nil {
				return p, SessionToken{}, err
			}
			return p, connectSessionParent(parent), nil
		}
	}
	return p, SessionToken{}, errAPIUnauthorized
}

func (s *Store) parentAmbiguousLocked(p Project, id string) bool {
	if _, ok := s.api.tokens[id]; !ok {
		return false
	}
	for _, t := range p.Tokens {
		if t.ID == id && t.Kind == tokenProject {
			return true
		}
	}
	return false
}
