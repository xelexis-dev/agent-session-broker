package controlpanel

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"maps"
	"regexp"
	"sort"
	"time"
)

// User is a public account view. Verifiers are deliberately kept in a separate
// persistence record and can never be returned by listing or authentication.
type User struct {
	DisplayName string `json:"displayName,omitempty"`
	Email       string `json:"email,omitempty"`
	ID          string `json:"id"`
	Username    string `json:"username"`
	Role        string `json:"role"`
	Enabled     bool   `json:"enabled"`
	Revision    uint64 `json:"revision"`
}
type userRecord struct {
	User
	Password passwordVerifier `json:"password"`
}

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

const unusedTokenLifetime = 30 * time.Minute

func validVerifier(p passwordVerifier) bool {
	salt, e1 := hex.DecodeString(p.Salt)
	hash, e2 := hex.DecodeString(p.Hash)
	return e1 == nil && e2 == nil && len(salt) == 16 && len(hash) == 32
}
func validUsers(st state) bool {
	if len(st.Users) == 0 || len(st.Users) > 512 {
		return false
	}
	admin, ok := st.Users["admin"]
	if !ok || !admin.Enabled || admin.Role != "admin" || admin.Password != *st.Admin {
		return false
	}
	for id, u := range st.Users {
		if id != u.ID || id != u.Username || !usernamePattern.MatchString(id) || (u.Role != "admin" && u.Role != "user") || u.Revision == 0 || !validVerifier(u.Password) || !validProfile(u.DisplayName, u.Email) {
			return false
		}
	}
	return true
}
func validMembers(st state, p Project) bool {
	if len(p.Members) == 0 {
		return false
	}
	owners := 0
	for id, role := range p.Members {
		u, ok := st.Users[id]
		if !ok || roleRank(role) == 0 {
			return false
		}
		if role == "owner" && u.Enabled {
			owners++
		}
	}
	return owners > 0
}
func roleRank(role string) int {
	switch role {
	case "owner":
		return 3
	case "developer":
		return 2
	case "viewer":
		return 1
	}
	return 0
}

func (s *Store) migrate() error {
	candidate := s.data
	candidate.Version = 2
	candidate.Users = map[string]userRecord{"admin": {User: User{ID: "admin", Username: "admin", Role: "admin", Enabled: true, Revision: 1}, Password: *candidate.Admin}}
	candidate.Projects = maps.Clone(s.data.Projects)
	now := s.now().UTC()
	for name, p := range candidate.Projects {
		p.Members = map[string]string{"admin": "owner"}
		p.Tokens = append([]SessionToken(nil), p.Tokens...)
		for i := range p.Tokens {
			p.Tokens[i].OwnerID = "admin"
			p.Tokens[i].IssuedAt = now
			if p.Tokens[i].BoundIdentity != "" {
				p.Tokens[i].FirstConnectedAt = now
			}
		}
		candidate.Projects[name] = p
	}
	return s.commit(candidate)
}

func (s *Store) User(id string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.data.Users[id]
	return u.User, ok && !s.closed
}
func (s *Store) activeUserLocked(id string) (userRecord, bool) {
	u, ok := s.data.Users[id]
	return u, ok && u.Enabled && !s.closed
}
func (s *Store) isAdminLocked(id string) bool {
	u, ok := s.activeUserLocked(id)
	return ok && u.Role == "admin"
}
func (s *Store) AuthenticateUser(username, password string) (User, bool) {
	if len(password) > 1024 {
		return User{}, false
	}
	select {
	case s.hashSlot <- struct{}{}:
	default:
		return User{}, false
	}
	defer func() { <-s.hashSlot }()
	s.mu.RLock()
	u, ok := s.activeUserLocked(username)
	s.mu.RUnlock()
	verifier := u.Password
	if !ok {
		verifier = dummyAPIVerifier
	}
	salt, _ := hex.DecodeString(verifier.Salt)
	expected, _ := hex.DecodeString(verifier.Hash)
	actual := derivePasswordKey([]byte(password), salt)
	if subtle.ConstantTimeCompare(expected, actual) != 1 || !ok {
		return User{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	current, ok := s.activeUserLocked(username)
	if !ok || current.Revision != u.Revision {
		return User{}, false
	}
	return current.User, true
}
func hashPassword(password string) (passwordVerifier, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return passwordVerifier{}, ErrStorage
	}
	hash := derivePasswordKey([]byte(password), salt)
	return passwordVerifier{Salt: hex.EncodeToString(salt), Hash: hex.EncodeToString(hash)}, nil
}
func (s *Store) Users(actor string) ([]User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.isAdminLocked(actor) {
		return nil, ErrForbidden
	}
	result := make([]User, 0, len(s.data.Users))
	for _, u := range s.data.Users {
		result = append(result, u.User)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Username < result[j].Username })
	return result, nil
}
func (s *Store) CreateUser(actor, username, password, role string) error {
	return s.createUserGuarded(actor, username, password, role, nil)
}
func (s *Store) createUserGuarded(actor, username, password, role string, guard commitGuard) error {
	if !usernamePattern.MatchString(username) || len(password) < 12 || len(password) > 1024 || (role != "admin" && role != "user") {
		return ErrInvalid
	}
	s.hashSlot <- struct{}{}
	defer func() { <-s.hashSlot }()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createUserLocked(actor, username, password, role, guard)
}

// createUserLocked requires Store.mu and hashSlot; re-auth callers must not acquire either twice.
func (s *Store) createUserLocked(actor, username, password, role string, guard commitGuard) error {
	if !usernamePattern.MatchString(username) || len(password) < 12 || len(password) > 1024 || (role != "admin" && role != "user") {
		return ErrInvalid
	}
	if !s.isAdminLocked(actor) {
		return ErrForbidden
	}
	if _, ok := s.data.Users[username]; ok || len(s.data.Users) >= 512 {
		return ErrInvalid
	}
	verifier, err := hashPassword(password)
	if err != nil {
		return err
	}
	candidate := s.data
	candidate.Users = maps.Clone(s.data.Users)
	candidate.Users[username] = userRecord{User: User{ID: username, Username: username, Role: role, Enabled: true, Revision: 1}, Password: verifier}
	if err := guard.check(); err != nil {
		return err
	}
	return s.commit(candidate)
}
func (s *Store) UpdateUser(actor, id, role string, enabled bool, password string, version ...string) error {
	return s.updateUserGuarded(actor, id, role, enabled, password, nil, version...)
}
func (s *Store) updateUserGuarded(actor, id, role string, enabled bool, password string, guard func(userChange) error, version ...string) error {
	s.hashSlot <- struct{}{}
	defer func() { <-s.hashSlot }()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateUserLocked(actor, id, role, enabled, password, guard, version...)
}

// updateUserLocked requires Store.mu and hashSlot; re-auth callers must not acquire either twice.
func (s *Store) updateUserLocked(actor, id, role string, enabled bool, password string, guard func(userChange) error, version ...string) error {
	if !s.isAdminLocked(actor) {
		return ErrForbidden
	}
	old, ok := s.data.Users[id]
	if !ok || (id == "admin" && (!enabled || role != "admin")) {
		return ErrInvalid
	}
	if err := checkSettingsVersion(version, userSettingsVersion(old.User)); err != nil {
		return err
	}
	if (role != "admin" && role != "user") || (password != "" && (len(password) < 12 || len(password) > 1024)) {
		return ErrInvalid
	}
	candidate := s.data
	candidate.Users = maps.Clone(s.data.Users)
	candidate.Projects = maps.Clone(s.data.Projects)
	u := old
	u.Role = role
	u.Enabled = enabled
	if password != "" {
		verifier, err := hashPassword(password)
		if err != nil {
			return err
		}
		u.Password = verifier
	}
	change := userChange{Before: old.User, After: u.User, PasswordSet: password != "", RaisesPower: (old.Role != "admin" && u.Role == "admin") || (!old.Enabled && u.Enabled) || password != ""}
	if u != old {
		change.After.Revision++
	}
	if guard != nil {
		if err := guard(change); err != nil {
			return err
		}
	}
	if u == old {
		return nil
	}
	u.Revision++
	candidate.Users[id] = u
	if id == "admin" {
		verifier := u.Password
		candidate.Admin = &verifier
	}
	for name, p := range candidate.Projects {
		if !validMembers(candidate, p) {
			return ErrInvalid
		}
		if !enabled || (old.Role == "admin" && role != "admin") {
			p.Tokens = revokeOwned(p.Tokens, id)
			candidate.Projects[name] = p
		}
	}
	if err := s.commit(candidate); err != nil {
		return err
	}
	for name, p := range s.data.Projects {
		if _, member := p.Members[id]; member || old.Role == "admin" {
			s.changed(name)
		}
	}
	return nil
}
func revokeOwned(tokens []SessionToken, id string) []SessionToken {
	result := append([]SessionToken(nil), tokens...)
	for i := range result {
		if result[i].OwnerID == id {
			result[i].Revoked = true
		}
	}
	return result
}

func (s *Store) projectRoleLocked(actor, project string) string {
	p, exists := s.data.Projects[project]
	if !exists {
		return ""
	}
	u, ok := s.activeUserLocked(actor)
	if !ok {
		return ""
	}
	if u.Role == "admin" {
		return "owner"
	}
	return p.Members[actor]
}
func (s *Store) ProjectRole(actor, project string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.projectRoleLocked(actor, project)
}
func (s *Store) CreateProjectFor(actor, name string) error {
	return s.saveProjectFor(actor, name, "token", true, true, false)
}
func (s *Store) SaveProjectFor(actor, name, mode string, enabled bool, version ...string) error {
	return s.saveProjectFor(actor, name, mode, enabled, false, false, version...)
}
func (s *Store) saveProjectFor(actor, name, mode string, enabled, create, upsert bool, version ...string) error {
	return s.saveProjectForGuarded(actor, name, mode, enabled, create, upsert, nil, version...)
}
func (s *Store) saveProjectForGuarded(actor, name, mode string, enabled, create, upsert bool, guard commitGuard, version ...string) error {
	if !projectName.MatchString(name) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveProjectForLocked(actor, name, mode, enabled, create, upsert, guard, version...)
}

// saveProjectForLocked requires Store.mu; API PATCH reads omitted fields in this lock.
func (s *Store) saveProjectForLocked(actor, name, mode string, enabled, create, upsert bool, guard commitGuard, version ...string) error {
	if !projectName.MatchString(name) {
		return ErrInvalid
	}
	if err := guard.check(); err != nil {
		return err
	}
	if s.closed || s.data.Admin == nil {
		return ErrStorage
	}
	if _, ok := s.activeUserLocked(actor); !ok {
		return ErrForbidden
	}
	old, exists := s.data.Projects[name]
	if create && exists || !exists && len(s.data.Projects) >= 128 {
		return ErrInvalid
	}
	if exists {
		if s.projectRoleLocked(actor, name) != "owner" {
			return ErrForbidden
		}
	} else if !create && !upsert {
		return ErrInvalid
	}
	if err := checkSettingsVersion(version, projectSettingsVersion(old)); err != nil {
		return err
	}
	if mode != "token" {
		return ErrInvalid
	}
	p := old
	p.Name = name
	p.Mode = mode
	p.Enabled = enabled
	p.UpdatedAt = s.now().UTC()
	if !exists {
		p.ProjectGenerationID = rand.Text()
		p.Members = map[string]string{actor: "owner"}
	}
	if err := s.commitProject(p); err != nil {
		return err
	}
	if exists && (old.Mode != mode || old.Enabled != enabled) {
		s.changed(name)
	}
	return nil
}
func (s *Store) SetProjectMember(actor, project, user, role string) error {
	return s.setProjectMember(actor, project, user, role, "")
}

func (s *Store) setProjectMember(actor, project, user, role, action string) error {
	_, err := s.changeProjectMember(actor, project, user, role, action)
	return err
}

// changeProjectMember reports whether the mutation committed under the same lock.
func (s *Store) changeProjectMember(actor, project, user, role, action string) (bool, error) {
	return s.changeProjectMemberGuarded(actor, project, user, role, action, nil)
}
func (s *Store) changeProjectMemberGuarded(actor, project, user, role, action string, guard commitGuard) (bool, error) {
	if role != "" && roleRank(role) == 0 {
		return false, ErrInvalid
	}
	if (action != "" && action != "add" && action != "update" && action != "remove") || ((action == "add" || action == "update") && role == "") {
		return false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := guard.check(); err != nil {
		return false, err
	}
	if s.projectRoleLocked(actor, project) != "owner" {
		return false, ErrForbidden
	}
	if _, ok := s.data.Users[user]; !ok {
		return false, ErrInvalid
	}
	p := s.data.Projects[project]
	old := p.Members[user]
	// Check under the mutation lock: stale add requests must never change an
	// existing member's role, especially an Owner's.
	if action == "add" && (old != "" || !s.data.Users[user].Enabled) || (action == "update" || action == "remove") && old == "" {
		return false, ErrInvalid
	}
	if old == role {
		return false, nil
	}
	p.Members = maps.Clone(p.Members)
	if role == "" {
		delete(p.Members, user)
	} else {
		p.Members[user] = role
	}
	if !validMembers(s.data, p) {
		return false, ErrInvalid
	}
	if roleRank(role) < roleRank(old) {
		p.Tokens = revokeOwned(p.Tokens, user)
	}
	p.UpdatedAt = s.now().UTC()
	if err := s.commitProject(p); err != nil {
		return false, err
	}
	s.changed(project)
	return true, nil
}
func (s *Store) ProjectsFor(actor string) []Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.expireLocked() != nil {
		return nil
	}
	return s.projectsForLocked(actor)
}

// Editor preview must not publish expiry maintenance as a side effect of rendering.
func (s *Store) projectSnapshotsFor(actor string) []Project {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil
	}
	return s.projectsForLocked(actor)
}
func (s *Store) projectsForLocked(actor string) []Project {
	projects := make([]Project, 0, len(s.data.Projects))
	for name, p := range s.data.Projects {
		role := s.projectRoleLocked(actor, name)
		if role == "" {
			continue
		}
		p.Members = maps.Clone(p.Members)
		// Device authorization records hold code hashes; they never leave the store.
		p.Devices = nil
		visible := make([]SessionToken, 0, len(p.Tokens))
		for _, token := range p.Tokens {
			if role == "owner" || token.OwnerID == actor {
				token.TokenHash = ""
				token.Scopes = append([]string(nil), token.Scopes...)
				visible = append(visible, token)
			}
		}
		p.Tokens = visible
		projects = append(projects, p)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	return projects
}
func (s *Store) tokenOwnerEligibleLocked(p Project, id string) bool {
	user, ok := s.activeUserLocked(id)
	return ok && (user.Role == "admin" || roleRank(p.Members[id]) >= 2)
}
func (s *Store) IssueTokenFor(actor, project, nickname, ownerID string) (string, error) {
	if !validNickname(nickname) {
		return "", ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.data.Admin == nil {
		return "", ErrStorage
	}
	role := s.projectRoleLocked(actor, project)
	if roleRank(role) < 2 {
		return "", ErrForbidden
	}
	if ownerID != "" && ownerID != actor {
		return "", ErrForbidden
	}
	if err := s.expireLocked(); err != nil {
		return "", err
	}
	p := s.data.Projects[project]
	if p.Mode != "token" || !p.Enabled {
		return "", ErrInvalid
	}
	if activeTokenCount(p.Tokens, s.now()) >= projectTokenCap {
		return "", ErrTokenCap
	}
	if !s.tokenOwnerEligibleLocked(p, actor) {
		return "", ErrForbidden
	}
	for _, token := range p.Tokens {
		if tokenReservesName(token, nickname) {
			return "", ErrInvalid
		}
	}
	raw := rand.Text() + rand.Text()
	p.Tokens = append(append([]SessionToken(nil), p.Tokens...), SessionToken{ScopeVersion: access.ScopeVersion, Scopes: access.LegacyV1Scopes(), ID: rand.Text() + rand.Text(), Nickname: nickname, TokenHash: digest(raw), OwnerID: actor, IssuedAt: s.now().UTC()})
	if err := s.commitProject(p); err != nil {
		return "", err
	}
	return raw, nil
}
func (s *Store) RevokeTokenFor(actor, project, id string) error {
	return s.revokeTokenForGuarded(actor, project, id, nil)
}
func (s *Store) revokeTokenForGuarded(actor, project, id string, guard commitGuard) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := guard.check(); err != nil {
		return err
	}
	if s.closed || s.data.Admin == nil {
		return ErrStorage
	}
	role := s.projectRoleLocked(actor, project)
	if roleRank(role) < 2 {
		return ErrForbidden
	}
	p := s.data.Projects[project]
	for i, token := range p.Tokens {
		if token.ID != id {
			continue
		}
		if role != "owner" && token.OwnerID != actor {
			return ErrForbidden
		}
		if token.Revoked {
			return nil
		}
		p.Tokens = append([]SessionToken(nil), p.Tokens...)
		p.Tokens[i].Revoked = true
		// A project token carries every session grant approved through it.
		if token.Kind == tokenProject {
			for j := range p.Tokens {
				if p.Tokens[j].ParentID == token.ID {
					p.Tokens[j].Revoked = true
				}
			}
		}
		if err := s.commitProject(p); err != nil {
			return err
		}
		s.changed(project)
		return nil
	}
	return ErrInvalid
}
func (s *Store) CanManageSession(actor, project, identity string, recordedOwner ...string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.expireLocked() != nil {
		return false
	}
	return s.canManageSessionLocked(actor, project, identity, recordedOwner...)
}

func (s *Store) canManageSessionLocked(actor, project, identity string, recordedOwner ...string) bool {
	if s.projectRoleLocked(actor, project) == "owner" {
		return true
	}
	if identity == "" {
		return false
	}
	tokens := s.data.Projects[project].Tokens
	for i := len(tokens) - 1; i >= 0; i-- {
		token := tokens[i]
		// Use the latest approval for this identity, matching SessionOwners.
		// An older revoked grant must not confer access after another member
		// reapproves the same conversation under their own token.
		if token.BoundIdentity == identity {
			return s.canManageOwnedSessionLocked(actor, project, token.OwnerID)
		}
	}
	if len(recordedOwner) == 1 {
		return s.canManageOwnedSessionLocked(actor, project, recordedOwner[0])
	}
	return false
}

// Shared by registered-session and reserved-grant management. Call with s.mu held.
func (s *Store) canManageOwnedSessionLocked(actor, project, ownerID string) bool {
	role := s.projectRoleLocked(actor, project)
	if role == "owner" {
		return true
	}
	p := s.data.Projects[project]
	return role == "developer" && p.Enabled && p.Mode == "token" && ownerID == actor
}

// SessionOwners names the account behind each bound session identity, using
// retained ownership bindings even after revocation. Displayed ownership does
// not grant management rights; those require current membership and role.
func (s *Store) SessionOwners(project string, sessions ...Session) map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return map[string]string{}
	}
	return s.sessionOwnersLocked(project, sessions)
}

func (s *Store) sessionOwnersLocked(project string, sessions []Session) map[string]string {
	ids := map[string]string{}
	for _, session := range sessions {
		if identity := sessionIdentity(session); identity != "" {
			ids[identity] = session.OwnerID
		}
	}
	for _, token := range s.data.Projects[project].Tokens {
		if token.BoundIdentity != "" {
			ids[token.BoundIdentity] = token.OwnerID
		}
	}
	owners := map[string]string{}
	for identity, id := range ids {
		if user, ok := s.data.Users[id]; ok {
			owners[identity] = user.Username
		}
	}
	return owners
}

// RegistrationOwnerBindings takes a read-only snapshot before retention cleanup
// for startup migration. Values are server grant owner IDs, including revoked grants.
func (s *Store) RegistrationOwnerBindings() map[string]map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := map[string]map[string]string{}
	if s.closed {
		return result
	}
	for name, p := range s.data.Projects {
		ids := map[string]string{}
		for _, token := range p.Tokens {
			if token.BoundIdentity != "" {
				ids[token.BoundIdentity] = token.OwnerID
			}
		}
		result[name] = ids
	}
	return result
}

// Avoid copying project/token collections when maintenance has nothing to publish.
func (s *Store) expiryPendingLocked(now time.Time) bool {
	for _, p := range s.data.Projects {
		for _, binding := range p.AutoBindings {
			if binding.State == "issued" && !now.Before(binding.ExpiresAt) {
				return true
			}
		}
		for _, exchange := range p.AutoExchanges {
			if !now.Before(exchange.ExpiresAt) {
				return true
			}
		}
		for _, token := range p.Tokens {
			if token.Revoked && token.RevokedAt.IsZero() || token.ExpiredAt.IsZero() && tokenExpired(token, now) {
				return true
			}
			retired := token.ExpiredAt
			if !token.RevokedAt.IsZero() && (retired.IsZero() || token.RevokedAt.Before(retired)) {
				retired = token.RevokedAt
			}
			if !retired.IsZero() && !now.Before(retired.Add(tokenHistoryRetention)) {
				return true
			}
		}
		for _, device := range p.Devices {
			if !now.Before(device.ExpiresAt) {
				return true
			}
		}
	}
	return false
}

// expireLocked publishes all expiration removals together, retaining the old
// state on disk failures. Callers fail closed when an expiration cannot persist.
func (s *Store) expireLocked() error {
	now := s.now().UTC()
	s.pruneDeviceDirectoryHintsLocked(now)
	if !s.expiryPendingLocked(now) {
		return nil
	}
	candidate := s.data
	candidate.Projects = maps.Clone(s.data.Projects)
	changed := []string{}
	for name, p := range s.data.Projects {
		updated := false
		for _, b := range p.AutoBindings {
			if b.State == "issued" && !now.Before(b.ExpiresAt) {
				updated = true
			}
		}
		for _, e := range p.AutoExchanges {
			if !now.Before(e.ExpiresAt) {
				updated = true
			}
		}
		kept := make([]SessionToken, 0, len(p.Tokens))
		for _, token := range p.Tokens {
			if token.Revoked && token.RevokedAt.IsZero() {
				token.RevokedAt = now
				updated = true
			}
			if token.ExpiredAt.IsZero() && tokenExpired(token, now) {
				token.ExpiredAt = tokenExpiry(token)
				updated = true
			}
			retired := token.ExpiredAt
			if !token.RevokedAt.IsZero() && (retired.IsZero() || token.RevokedAt.Before(retired)) {
				retired = token.RevokedAt
			}
			if !retired.IsZero() && !now.Before(retired.Add(tokenHistoryRetention)) {
				continue
			}
			kept = append(kept, token)
		}
		devices := make([]DeviceAuthorization, 0, len(p.Devices))
		for _, device := range p.Devices {
			if now.Before(device.ExpiresAt) {
				devices = append(devices, device)
			}
		}
		if updated || len(kept) != len(p.Tokens) || len(devices) != len(p.Devices) {
			p.Tokens = kept
			if len(devices) == 0 {
				devices = nil
			}
			p.Devices = devices
			candidate.Projects[name] = p
			changed = append(changed, name)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	previous := s.data
	if err := s.commit(candidate); err != nil {
		return err
	}
	for _, name := range changed {
		tokens := s.data.Projects[name].Tokens
		s.notifyTokenChangesLocked(name, tokens)
		// Maintenance and session credentials have token-local lifetime signals.
		// Only newly unusable non-session tokens need to cancel the generation.
		newlyUnusable := make(map[string]bool)
		for _, token := range previous.Projects[name].Tokens {
			if token.Kind != tokenSession && !token.Revoked && token.ExpiredAt.IsZero() {
				newlyUnusable[token.ID] = true
			}
		}
		for _, token := range tokens {
			if !token.Revoked && token.ExpiredAt.IsZero() {
				delete(newlyUnusable, token.ID)
			}
		}
		if len(newlyUnusable) > 0 {
			s.changed(name)
		}
	}
	return nil
}

// tokenExpired applies the lifetime rule of each token kind: legacy tokens
// expire unused after unusedTokenLifetime, session credentials at their
// absolute expiry or after an idle period, and project tokens only by revoke.
func tokenExpired(token SessionToken, now time.Time) bool {
	if !token.ExpiredAt.IsZero() {
		return true
	}
	expires := tokenExpiry(token)
	return !expires.IsZero() && !now.Before(expires)
}

func tokenExpiry(token SessionToken) time.Time {
	switch token.Kind {
	case tokenProject:
		return token.ExpiresAt
	case tokenSession:
		last := token.LastUsedAt
		if last.IsZero() {
			last = token.IssuedAt
		}
		idle := last.Add(sessionIdleLifetime)
		if idle.Before(token.ExpiresAt) {
			return idle
		}
		return token.ExpiresAt
	default:
		if token.FirstConnectedAt.IsZero() {
			return token.IssuedAt.Add(unusedTokenLifetime)
		}
		return time.Time{}
	}
}

func (s *Store) SweepExpired() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStorage
	}
	if err := s.expireLocked(); err != nil {
		return err
	}
	if err := s.sweepAPILocked(); err != nil {
		return err
	}
	return s.pruneDirectAuditLocked()
}
func (s *Store) sweepLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			_ = s.SweepExpired()
		}
	}
}
