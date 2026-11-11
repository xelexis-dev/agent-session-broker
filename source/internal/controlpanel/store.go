// Package controlpanel owns administrator credentials and project access policy.
// It never observes or starts local harness processes.
package controlpanel

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/attachments"
	"maps"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

var (
	ErrSetupCode         = errors.New("invalid setup code")
	ErrAlreadySetup      = errors.New("administrator is already configured")
	ErrForbidden         = errors.New("permission denied")
	ErrInvalid           = errors.New("invalid configuration")
	ErrStorage           = errors.New("control panel storage unavailable")
	registrationNickname = regexp.MustCompile(`^[A-Z2-7]{26}$`)
	projectName          = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

type Project struct {
	ProjectGenerationID string                   `json:"projectGenerationId,omitempty"`
	Rebinds             []protocol.RebindJournal `json:"rebinds,omitempty"`

	AutoBindings  []AutoBinding  `json:"autoBindings,omitempty"`
	AutoExchanges []AutoExchange `json:"autoExchanges,omitempty"`
	// AllowHTTP is the 0.7.0 per-project opt-in, kept only so older state
	// decodes; the live policy is state.AllowHTTP.
	AllowHTTP bool                  `json:"allowHTTP,omitempty"`
	Devices   []DeviceAuthorization `json:"devices,omitempty"`
	Deleting  bool                  `json:"deleting,omitempty"`
	Members   map[string]string     `json:"members"`
	Name      string                `json:"name"`
	Mode      string                `json:"mode"`
	Enabled   bool                  `json:"enabled"`
	Tokens    []SessionToken        `json:"tokens,omitempty"`
	UpdatedAt time.Time             `json:"updatedAt"`
}

// SessionToken reserves a nickname until revoked or its unused lifetime ends.
type SessionToken struct {
	AdmissionMode   string   `json:"admissionMode,omitempty"`
	IssuedBy        string   `json:"issuedBy,omitempty"`
	MaxAutoBindings int      `json:"maxAutoBindings,omitempty"`
	ScopeVersion    int      `json:"scopeVersion,omitempty"`
	Scopes          []string `json:"scopes,omitempty"`
	Kind            string   `json:"kind,omitempty"`
	Label           string   `json:"label,omitempty"`
	// Approvals and LastRegisteredAt keep a project token's history even after
	// its session grants are revoked or expire and are swept away.
	Approvals        int       `json:"approvals,omitempty"`
	LastRegisteredAt time.Time `json:"lastRegisteredAt,omitempty"`
	ParentID         string    `json:"parentID,omitempty"`
	ExpiresAt        time.Time `json:"expiresAt,omitempty"`
	FirstUsedAt      time.Time `json:"firstUsedAt,omitempty"`
	LastUsedAt       time.Time `json:"lastUsedAt,omitempty"`
	OwnerID          string    `json:"ownerID"`
	IssuedAt         time.Time `json:"issuedAt"`
	FirstConnectedAt time.Time `json:"firstConnectedAt,omitempty"`
	ID               string    `json:"id"`
	Nickname         string    `json:"nickname"`
	PendingNickname  string    `json:"pendingNickname,omitempty"`
	TokenHash        string    `json:"tokenHash,omitempty"`
	BoundIdentity    string    `json:"boundIdentity,omitempty"`
	Revoked          bool      `json:"revoked"`
	RevokedAt        time.Time `json:"revokedAt,omitempty"`
	ExpiredAt        time.Time `json:"expiredAt,omitempty"`
}

type passwordVerifier struct {
	Salt string `json:"salt"`
	Hash string `json:"hash"`
}

type state struct {
	ScopeIssuanceReady   policyBool            `json:"scopeIssuanceReady,omitempty"`
	ScopeIssuanceChanges []ScopeIssuanceChange `json:"scopeIssuanceChanges,omitempty"`
	// AllowHTTP is the instance-wide opt-in for clients to send project
	// and session credentials over plain HTTP. Project-level opt-ins from 0.7.0
	// are folded into it once by migrateHTTPPolicy.
	AllowHTTP              bool                  `json:"allowHTTP,omitempty"`
	DisableUnauthenticated bool                  `json:"disableUnauthenticated,omitempty"`
	Users                  map[string]userRecord `json:"users,omitempty"`
	Version                int                   `json:"version"`
	Admin                  *passwordVerifier     `json:"admin"`
	Projects               map[string]Project    `json:"projects"`
}

type Store struct {
	languages           map[string]string
	invalidUserSettings int
	api                 apiState
	listRecords         func() (map[string][]byte, error)

	attachmentStore        *attachments.Store
	attachmentEnabled      bool
	attachmentLastChange   *attachmentSetting
	attachmentDiskCapacity func() (attachments.Capacity, error)
	beforeTokenExchange    func() // Test barrier before taking the issuance/policy lock.
	deviceDirectoryHints   map[string]deviceDirectoryHint
	writeBatch             func(map[string][]byte, []string) error
	audits                 map[string]AuditEvent
	directRates            map[string]directRate
	devicePoll             map[string]devicePollState
	tokenChanges           map[string]chan struct{}
	operations             map[string]*projectOperations
	now                    func() time.Time
	stop                   chan struct{}
	mu                     sync.RWMutex
	setupCode              string
	data                   state
	root                   *os.Root
	lock                   *os.File
	write                  func([]byte) error
	closeDatabase          func() error
	closed                 bool
	changes                map[string]chan struct{}
	// Hash concurrency is bounded independently of browser request count.
	hashSlot chan struct{}
}

func openControlRoot(directory string) (*Store, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, ErrStorage
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, ErrStorage
	}
	s := &Store{now: time.Now, stop: make(chan struct{}), changes: map[string]chan struct{}{}, root: root, hashSlot: make(chan struct{}, 1), data: state{DisableUnauthenticated: true, Version: controlVersion, Users: map[string]userRecord{}, Projects: map[string]Project{}}}
	fail := func() (*Store, error) { s.Close(); return nil, ErrStorage }
	// Do not follow a user-replaced lock file outside this state directory.
	if info, e := root.Lstat("control.lock"); e == nil && !info.Mode().IsRegular() {
		return fail()
	}
	s.lock, err = root.OpenFile("control.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fail()
	}
	if err = lockFile(s.lock); err != nil {
		return fail()
	}
	return s, nil
}

func validState(st state) bool {
	if (st.Version != 1 && st.Version != 2 && st.Version != 3 && st.Version != scopeIntroductionVersion && st.Version != controlVersion) || st.Admin == nil || st.Projects == nil || len(st.Projects) > 128 {
		return false
	}
	if !validProjectGenerations(st) || !validScopePolicy(st) || !validScopeState(st) {
		return false
	}
	salt, e1 := hex.DecodeString(st.Admin.Salt)
	hash, e2 := hex.DecodeString(st.Admin.Hash)
	if e1 != nil || e2 != nil || len(salt) != 16 || len(hash) != 32 {
		return false
	}
	if st.Version >= 2 && !validUsers(st) {
		return false
	}
	now := time.Now().UTC()
	for name, p := range st.Projects {
		if p.Deleting && (p.Enabled || len(p.Tokens) != 0) {
			return false
		}
		if !validRebindJournals(p.Rebinds) || name != p.Name || !projectName.MatchString(name) || (p.Mode != "open" && p.Mode != "token") {
			return false
		}
		if st.Version >= 2 && !validMembers(st, p) {
			return false
		}
		if activeTokenCount(p.Tokens, now) > projectTokenCap {
			return false
		}
		ids, names, hashes := map[string]bool{}, map[string]bool{}, map[string]bool{}
		for _, token := range p.Tokens {
			if st.Version >= 2 {
				if _, ok := st.Users[token.OwnerID]; !ok || token.IssuedAt.IsZero() || (!token.FirstConnectedAt.IsZero() && token.BoundIdentity == "") {
					return false
				}
			}
			h, err := hex.DecodeString(token.TokenHash)
			if (token.Kind != tokenProject && !validNickname(token.Nickname)) || !validDeviceToken(token, st.Version) || len(token.ID) != 52 || ids[token.ID] || hashes[token.TokenHash] || err != nil || len(h) != 32 || len(token.BoundIdentity) > 1024 {
				return false
			}
			if token.Kind != tokenProject && tokenActive(token, now) && names[token.Nickname] {
				return false
			}
			ids[token.ID] = true
			hashes[token.TokenHash] = true
			if token.Kind != tokenProject && tokenActive(token, now) {
				names[token.Nickname] = true
			}
			if token.PendingNickname != "" {
				if token.Kind == tokenProject || !validNickname(token.PendingNickname) || token.PendingNickname == token.Nickname || token.BoundIdentity == "" || (tokenActive(token, now) && names[token.PendingNickname]) {
					return false
				}
				if tokenActive(token, now) {
					names[token.PendingNickname] = true
				}
			}
		}
		if !validDevices(p) {
			return false
		}
	}
	return true
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.stop != nil {
		close(s.stop)
	}
	for _, ch := range s.changes {
		close(ch)
	}
	s.changes = map[string]chan struct{}{}
	var databaseError error
	if s.closeDatabase != nil {
		databaseError = s.closeDatabase()
	}
	if s.lock != nil {
		s.lock.Close()
	}
	if s.root != nil {
		return errors.Join(databaseError, s.root.Close())
	}
	return databaseError
}

func (s *Store) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.closed && s.data.Admin != nil
}

// Setup is for in-process initialization; HTTP must use SetupWithCode.
func (s *Store) Setup(password string) error { return s.setup(password, nil) }

func (s *Store) SetupWithCode(code, password string) error { return s.setup(password, &code) }

func (s *Store) checkSetupCode(code string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.checkSetupCodeLocked(code)
}

func (s *Store) checkSetupCodeLocked(code string) error {
	if s.closed {
		return ErrStorage
	}
	if s.data.Admin != nil {
		return ErrAlreadySetup
	}
	if !equal(s.setupCode, strings.ToUpper(strings.TrimSpace(code))) {
		return ErrSetupCode
	}
	return nil
}

func (s *Store) setup(password string, code *string) error {
	s.hashSlot <- struct{}{}
	defer func() { <-s.hashSlot }()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStorage
	}
	if s.data.Admin != nil {
		return ErrAlreadySetup
	}
	if code != nil {
		if err := s.checkSetupCodeLocked(*code); err != nil {
			return err
		}
	}
	if len(password) < 12 || len(password) > 1024 {
		return ErrInvalid
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return ErrStorage
	}
	hash := derivePasswordKey([]byte(password), salt)
	candidate := s.data
	candidate.Admin = &passwordVerifier{Salt: hex.EncodeToString(salt), Hash: hex.EncodeToString(hash)}
	candidate.Users = map[string]userRecord{"admin": {User: User{ID: "admin", Username: "admin", Role: "admin", Enabled: true, Revision: 1}, Password: *candidate.Admin}}
	if err := s.commit(candidate); err != nil {
		return err
	}
	s.setupCode = ""
	_ = s.root.Remove("setup-code")
	return nil
}

func (s *Store) Authenticate(password string) bool {
	_, ok := s.AuthenticateUser("admin", password)
	return ok
}

func digest(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// SaveProject changes access policy. The legacy token argument must be empty;
// session credentials are generated by IssueToken, never supplied by a form.
func (s *Store) SaveProject(name, mode string, enabled bool, token string) error {
	if token != "" {
		return ErrInvalid
	}
	return s.saveProjectFor("admin", name, mode, enabled, false, true)
}
func (s *Store) CreateProject(name string) error { return s.CreateProjectFor("admin", name) }
func (s *Store) commitProject(p Project) error {
	if old, ok := s.data.Projects[p.Name]; ok && old.Deleting {
		return ErrForbidden
	}
	candidate := s.data
	for _, token := range p.Tokens {
		if token.Kind != "" && candidate.Version < 3 {
			candidate.Version = 3
			break
		}
	}
	candidate.Projects = make(map[string]Project, len(s.data.Projects)+1)
	for k, v := range s.data.Projects {
		candidate.Projects[k] = v
	}
	candidate.Projects[p.Name] = p
	if err := s.commit(candidate); err != nil {
		return err
	}
	s.notifyTokenChangesLocked(p.Name, p.Tokens)
	return nil
}
func validNickname(name string) bool {
	if protocol.IsAdminSender(name) || name == "" || len(name) > 128 || !utf8.ValidString(name) || registrationNickname.MatchString(name) || strings.HasPrefix(name, "name:") || strings.HasPrefix(name, "session:") || strings.HasPrefix(name, "registration:") {
		return false
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return false
		}
	}
	return true
}
func (s *Store) IssueToken(project, nickname string) (string, error) {
	return s.IssueTokenFor("admin", project, nickname, "admin")
}
func (s *Store) RevokeToken(project, id string) error {
	return s.RevokeTokenFor("admin", project, id)
}

// changed closes one generation channel, with at most one live channel per project.
// Backend code is never called while the store lock is held.
func (s *Store) changed(project string) {
	if ch := s.changes[project]; ch != nil {
		close(ch)
		delete(s.changes, project)
	}
}
func (s *Store) Projects() []Project { return s.ProjectsFor("admin") }

// AllowHTTP reports whether a currently usable project has opted
// clients into sending credentials over HTTP. It deliberately fails closed.
func (s *Store) AllowHTTP(project string) bool {
	allow, ok := s.TransportPolicy(project)
	return ok && allow
}

// TransportPolicy returns the instance HTTP opt-in for an enabled, non-deleting
// project. The second result intentionally combines missing and unavailable
// projects so the public endpoint reveals nothing about them.
func (s *Store) TransportPolicy(project string) (bool, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.data.Projects[project]
	if s.closed || s.data.Admin == nil || !ok || !p.Enabled || p.Deleting {
		return false, false
	}
	return s.data.AllowHTTP, true
}

// InstanceHTTPPolicy reports the instance-wide HTTP opt-in for the admin UI.
func (s *Store) InstanceHTTPPolicy() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.closed && s.data.Admin != nil && s.data.AllowHTTP
}

// SetInstanceHTTPPolicy changes the credential-transport opt-in for every
// project. Only a system administrator may make this security decision, and
// live streams are interrupted only after the new policy is durable.
func (s *Store) SetInstanceHTTPPolicy(actor string, allow bool, version ...string) error {
	return s.setInstanceHTTPPolicyGuarded(actor, allow, nil, version...)
}
func (s *Store) setInstanceHTTPPolicyGuarded(actor string, allow bool, guard commitGuard, version ...string) error {
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
	if err := checkSettingsVersion(version, policySettingsVersion("http-policy", s.data.AllowHTTP)); err != nil {
		return err
	}
	if s.data.AllowHTTP == allow {
		return nil
	}
	candidate := s.data
	candidate.AllowHTTP = allow
	if err := s.commit(candidate); err != nil {
		return err
	}
	for name := range s.data.Projects {
		s.changed(name)
	}
	return nil
}
func (s *Store) Authorize(project, token string) bool {
	_, _, ok := s.authorize(project, token)
	return ok
}

// authorize atomically captures policy and its cancellation generation, avoiding
// a gap where revocation could happen before a request starts watching changes.
func (s *Store) authorize(project, raw string) (access.Grant, <-chan struct{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.expireLocked() != nil {
		return access.Grant{}, nil, false
	}
	p, ok := s.data.Projects[project]
	if s.closed || s.data.Admin == nil || !ok || !p.Enabled || p.Deleting || p.Mode != "token" {
		return access.Grant{}, nil, false
	}
	if s.changes[project] == nil {
		s.changes[project] = make(chan struct{})
	}
	generation := s.changes[project]
	grant := access.Grant{Project: project, ProjectGenerationID: p.ProjectGenerationID}
	if raw == "" || len(raw) > 1024 {
		return access.Grant{}, nil, false
	}
	hash := digest(raw)
	for i, token := range p.Tokens {
		if token.Kind != tokenProject && s.sessionTokenEligibleLocked(p, token) && subtle.ConstantTimeCompare([]byte(token.TokenHash), []byte(hash)) == 1 {
			if token.Kind == tokenSession {
				now := s.now().UTC()
				if token.FirstUsedAt.IsZero() || now.Sub(token.LastUsedAt) >= time.Hour {
					p.Tokens = append([]SessionToken(nil), p.Tokens...)
					if token.FirstUsedAt.IsZero() {
						token.FirstUsedAt = now
					}
					token.LastUsedAt = now
					p.Tokens[i] = token
					// The parent keeps the latest use so expiry of this grant
					// cannot move the project token's last use backwards.
					for j := range p.Tokens {
						if p.Tokens[j].ID == token.ParentID {
							p.Tokens[j].LastUsedAt = now
						}
					}
					if s.commitProject(p) != nil {
						return access.Grant{}, nil, false
					}
				}
				grant.ExpiresAt = token.ExpiresAt
				for _, parent := range p.Tokens {
					if parent.ID == token.ParentID && !parent.ExpiresAt.IsZero() && parent.ExpiresAt.Before(grant.ExpiresAt) {
						grant.ExpiresAt = parent.ExpiresAt
					}
				}
			}
			grant.Revoked = s.tokenWatchLocked(project, token.ID)
			grant.Scopes = append([]string(nil), token.Scopes...)
			grant.OwnerID = token.OwnerID
			grant.ParentID, grant.AdmissionMode = token.ParentID, token.AdmissionMode
			if token.AdmissionMode == "auto" {
				id := token.ID
				grant.Registration = func(identity, registrationID, owner, name string, revision uint64, confirmed bool) error {
					return s.directRegistration(project, id, identity, registrationID, owner, name, revision, confirmed)
				}
			}
			grant.Rebind = func(phase string, journal protocol.RebindJournal, result protocol.RebindResult) error {
				return s.manualRebind(project, token.ID, phase, journal, result)
			}
			grant.TokenID = token.ID
			grant.Nickname = token.Nickname
			id := token.ID
			grant.Claim = func(identity string) error { return s.checkBinding(project, id, identity, generation, true) }
			grant.Check = func(identity string) error { return s.checkBinding(project, id, identity, generation, false) }
			grant.CheckName = func(identity, name string) error { return s.checkNickname(project, id, identity, name, generation) }
			grant.Rename = func(identity, oldName, newName string, apply func() error) error {
				return s.renameNickname(project, id, identity, oldName, newName, generation, apply)
			}
			grant.Connected = func(identity string) error { return s.confirmConnection(project, id, identity, generation) }
			grant.Manage = func(identity, recordedOwner string) error {
				return s.checkSessionManagement(project, id, identity, recordedOwner, generation)
			}
			grant.Retire = func(target access.RetirementTarget, detach func() error) ([]protocol.RevokedCredential, error) {
				return s.retireCredentials(project, id, target, generation, detach)
			}
			grant.ManagementAudit = func(event protocol.ManagementEvent) error {
				return s.RecordSessionManagement(project, token.OwnerID, event, p.ProjectGenerationID)
			}
			break
		}
	}
	if grant.TokenID == "" {
		return access.Grant{}, nil, false
	}
	return grant, generation, true
}
func (s *Store) checkBinding(project, id, identity string, generation <-chan struct{}, bind bool) error {
	if identity == "" || len(identity) > 1024 {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.expireLocked() != nil {
		return ErrInvalid
	}
	select {
	case <-generation:
		return ErrInvalid
	default:
	}
	p, ok := s.data.Projects[project]
	if s.closed || !ok || !p.Enabled || p.Mode != "token" {
		return ErrInvalid
	}
	for i, token := range p.Tokens {
		if token.ID == id && s.sessionTokenEligibleLocked(p, token) {
			if token.BoundIdentity != "" {
				if token.BoundIdentity == identity {
					return nil
				}
				return ErrInvalid
			}
			if !bind {
				return ErrInvalid
			}
			p.Tokens = append([]SessionToken(nil), p.Tokens...)
			p.Tokens[i].BoundIdentity = identity
			return s.commitProject(p)
		}
	}
	return ErrInvalid
}

// confirmConnection runs only after backend registration succeeds. Claiming an
// identity alone does not extend the unused credential's lifetime.
func (s *Store) confirmConnection(project, id, identity string, generation <-chan struct{}) error {
	if identity == "" || len(identity) > 1024 {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.expireLocked() != nil {
		return ErrInvalid
	}
	select {
	case <-generation:
		return ErrInvalid
	default:
	}
	p, ok := s.data.Projects[project]
	if !ok || !p.Enabled || p.Mode != "token" {
		return ErrInvalid
	}
	for i, token := range p.Tokens {
		if token.ID != id {
			continue
		}
		if !s.sessionTokenEligibleLocked(p, token) || token.BoundIdentity != identity {
			return ErrInvalid
		}
		if !token.FirstConnectedAt.IsZero() {
			return nil
		}
		now := s.now().UTC()
		p.Tokens = append([]SessionToken(nil), p.Tokens...)
		p.Tokens[i].FirstConnectedAt = now
		for j := range p.Tokens {
			if token.ParentID != "" && p.Tokens[j].ID == token.ParentID {
				p.Tokens[j].LastRegisteredAt = now
			}
		}
		return s.commitProject(p)
	}
	return ErrInvalid
}

func (s *Store) commit(candidate state) error { return s.commitAudited(candidate, nil) }

func (s *Store) commitAudited(candidate state, events []AuditEvent) error {
	return s.commitAuditedAPI(candidate, events, nil, nil)
}

func (s *Store) commitAuditedAPI(candidate state, events []AuditEvent, apiEvent *apiAuditEvent, extra map[string][]byte) error {
	candidate, events = s.prepareAutoCommit(candidate, events)
	events = s.auditAutoTransitions(candidate, events)
	if !validProjectGenerations(candidate) || !validAutoState(candidate) {
		return ErrStorage
	}
	if !validScopePolicy(candidate) || !validScopeState(candidate) {
		return ErrStorage
	}
	// Timestamp every revocation path, without modifying the published slices.
	candidate.Projects = maps.Clone(candidate.Projects)
	for name, p := range candidate.Projects {
		copied := false
		for i, token := range p.Tokens {
			if token.Revoked && token.RevokedAt.IsZero() {
				if !copied {
					p.Tokens = append([]SessionToken(nil), p.Tokens...)
					copied = true
				}
				p.Tokens[i].RevokedAt = s.now().UTC()
			}
		}
		candidate.Projects[name] = p
	}
	data, err := json.Marshal(candidate)
	if err != nil || len(data) > controlMaxBytes || (candidate.Version < scopeIntroductionVersion && len(data) > legacyControlMaxBytes) {
		return ErrStorage
	}
	puts := maps.Clone(extra)
	if puts == nil {
		puts = map[string][]byte{}
	}
	puts["state"] = data
	// Remove the previous project's audit in the same transaction as its
	// deletion. Also discard orphaned audit before a same-name recreation.
	pruneAudit := false
	for _, event := range s.audits {
		if pruneGenerationAudit(event, s.data.Projects, candidate.Projects) {
			pruneAudit = true
			break
		}
	}
	if len(events) > 0 || pruneAudit {
		if err = s.writeAuditBatch(puts, events, candidate.Projects, apiEvent); err != nil {
			return ErrStorage
		}
	} else if apiEvent != nil {
		if err = s.apiBatchLocked(puts, nil, *apiEvent); err != nil {
			return ErrStorage
		}
	} else if err = s.write(data); err != nil {
		return ErrStorage
	}
	s.data = candidate
	s.pruneDevicePollLocked()
	return nil
}

// ImportProjects adds legacy project directories without changing existing policy.
// Validation and persistence are atomic; an error never publishes a partial set.
func (s *Store) ImportProjects(names []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.data.Admin == nil {
		return ErrStorage
	}
	candidate := s.data
	candidate.Projects = make(map[string]Project, len(s.data.Projects))
	for k, v := range s.data.Projects {
		candidate.Projects[k] = v
	}
	for _, name := range names {
		if !projectName.MatchString(name) {
			return ErrInvalid
		}
		if _, exists := candidate.Projects[name]; !exists {
			candidate.Projects[name] = Project{ProjectGenerationID: rand.Text(), Name: name, Mode: "token", Enabled: true, Members: map[string]string{"admin": "owner"}, UpdatedAt: s.now().UTC()}
		}
		if len(candidate.Projects) > 128 {
			return ErrInvalid
		}
	}
	if len(candidate.Projects) == len(s.data.Projects) {
		return nil
	}
	return s.commit(candidate)
}
