package controlpanel

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"math/big"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	tokenProject        = "project"
	tokenSession        = "session"
	deviceLifetime      = 10 * time.Minute
	sessionLifetime     = 30 * 24 * time.Hour
	sessionIdleLifetime = 7 * 24 * time.Hour
)

type deviceError string

func (e deviceError) Error() string { return string(e) }

const (
	errDeviceClient  deviceError = "invalid_client"
	errDeviceRequest deviceError = "invalid_request"
	errDevicePending deviceError = "authorization_pending"
	errDeviceSlow    deviceError = "slow_down"
	errDeviceDenied  deviceError = "access_denied"
	errDeviceExpired deviceError = "expired_token"
	errDeviceGrant   deviceError = "invalid_grant"
)

var ErrNicknameTaken = errors.New("nickname already reserved")

type DeviceRequest struct {
	CWDBasename string `json:"-"`
	ClientID    string `json:"clientID"`
	Harness     string `json:"harness"`
	SessionID   string `json:"sessionID"`
	InstanceID  string `json:"instanceID"`
	RequestID   string `json:"requestID"`
}

func (r DeviceRequest) identity() string {
	b, _ := json.Marshal([3]string{r.Harness, r.SessionID, r.InstanceID})
	return string(b)
}
func (r DeviceRequest) valid() bool {
	return (r.CWDBasename == "" || (len(r.CWDBasename) <= 255 && utf8.ValidString(r.CWDBasename) && !strings.ContainsAny(r.CWDBasename, "/\\\x00\r\n"))) && r.ClientID == "asb-client-v1" && (r.Harness == "codex" || r.Harness == "claude" || r.Harness == "kimi") && safeDeviceText(r.SessionID, 128) && safeDeviceText(r.InstanceID, 128) && safeDeviceText(r.RequestID, 128)
}
func safeDeviceText(v string, max int) bool {
	return v != "" && len(v) <= max && utf8.ValidString(v) && !strings.ContainsAny(v, "\x00\r\n\t ")
}

type DeviceAuthorization struct {
	ScopeVersion int           `json:"scopeVersion,omitempty"`
	Scopes       []string      `json:"scopes,omitempty"`
	Request      DeviceRequest `json:"request"`
	TokenID      string        `json:"tokenID"`
	CodeHash     string        `json:"codeHash"`
	UserHash     string        `json:"userHash"`
	ExpiresAt    time.Time     `json:"expiresAt"`
	CreatedAt    time.Time     `json:"createdAt"`
	Status       string        `json:"status"`
	GrantID      string        `json:"grantID,omitempty"`
	ApprovedBy   string        `json:"approvedBy,omitempty"`
}
type DeviceResponse struct {
	VerificationURIComplete string `json:"verification_uri_complete"`
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}
type DeviceTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	GrantID     string `json:"session_grant_id"`
	Nickname    string `json:"nickname"`
}
type DeviceView struct {
	Project, OwnerID, Harness, InstanceID, SessionID, Status, Nickname string
	ClientID, CWDBasename                                              string
	CreatedAt, ExpiresAt                                               time.Time
}
type devicePollState struct {
	next     time.Time
	interval time.Duration
}

func validDeviceToken(t SessionToken, version int) bool {
	switch t.Kind {
	case "":
		return t.ParentID == "" && t.ExpiresAt.IsZero() && t.FirstUsedAt.IsZero() && t.LastUsedAt.IsZero()
	case tokenProject:
		return version >= 3 && t.Nickname == "" && t.BoundIdentity == "" && t.ParentID == "" && len(t.Label) <= 128
	case tokenSession:
		return version >= 3 && len(t.ParentID) == 52 && t.BoundIdentity != "" && !t.ExpiresAt.IsZero()
	default:
		return false
	}
}
func validDevices(p Project) bool {
	if len(p.Devices) > 32 {
		return false
	}
	codes, users := map[string]bool{}, map[string]bool{}
	for _, d := range p.Devices {
		if !d.Request.valid() || len(d.TokenID) != 52 || !validDeviceHash(d.CodeHash) || !validDeviceHash(d.UserHash) || codes[d.CodeHash] || users[d.UserHash] || d.ExpiresAt.IsZero() || d.CreatedAt.IsZero() {
			return false
		}
		if d.Status != "pending" && d.Status != "approved" && d.Status != "denied" {
			return false
		}
		if d.Status == "approved" && (len(d.GrantID) != 52 || d.ApprovedBy == "") {
			return false
		}
		codes[d.CodeHash] = true
		users[d.UserHash] = true
	}
	return true
}
func validDeviceHash(h string) bool { b, e := hex.DecodeString(h); return e == nil && len(b) == 32 }
func (s *Store) tokenWatchLocked(project, id string) <-chan struct{} {
	if s.tokenChanges == nil {
		s.tokenChanges = map[string]chan struct{}{}
	}
	key := project + "/" + id
	if s.tokenChanges[key] == nil {
		s.tokenChanges[key] = make(chan struct{})
	}
	return s.tokenChanges[key]
}

// notifyTokenChangesLocked wakes the streams of every watched token that is
// no longer usable after a commit: revoked, removed, or now expired.
func (s *Store) notifyTokenChangesLocked(project string, tokens []SessionToken) {
	usable := map[string]bool{}
	now := s.now()
	for _, token := range tokens {
		if !token.Revoked && !tokenExpired(token, now) {
			usable[project+"/"+token.ID] = true
		}
	}
	for key := range s.tokenChanges {
		if strings.HasPrefix(key, project+"/") && !usable[key] {
			close(s.tokenChanges[key])
			delete(s.tokenChanges, key)
		}
	}
}
func (s *Store) sessionTokenEligibleLocked(p Project, t SessionToken) bool {
	if !tokenActive(t, s.now()) || t.Kind == tokenProject || !s.tokenOwnerEligibleLocked(p, t.OwnerID) {
		return false
	}
	if t.Kind != tokenSession {
		return true
	}
	now := s.now()
	if !now.Before(t.ExpiresAt) || (!t.LastUsedAt.IsZero() && !now.Before(t.LastUsedAt.Add(sessionIdleLifetime))) {
		return false
	}
	for _, parent := range p.Tokens {
		if parent.ID == t.ParentID && parent.Kind == tokenProject && tokenActive(parent, now) && parent.OwnerID == t.OwnerID && s.tokenOwnerEligibleLocked(p, parent.OwnerID) {
			return true
		}
	}
	return false
}

// touchProjectToken returns a copy of tokens with the project token's last use
// stamped. Device authorization is rare, so every success is recorded.
func touchProjectToken(tokens []SessionToken, id string, now time.Time) []SessionToken {
	copied := append([]SessionToken(nil), tokens...)
	for i := range copied {
		if copied[i].ID == id {
			copied[i].LastUsedAt = now
		}
	}
	return copied
}
func (s *Store) projectTokenLocked(project, raw string) (Project, SessionToken, error) {
	p, ok := s.data.Projects[project]
	if s.closed || !ok || !p.Enabled || p.Deleting || p.Mode != "token" || !strings.HasPrefix(raw, "asb_p_") || len(raw) > 256 {
		return p, SessionToken{}, errDeviceClient
	}
	h := digest(raw)
	for _, t := range p.Tokens {
		if t.Kind == tokenProject && tokenActive(t, s.now()) && equal(t.TokenHash, h) && s.tokenOwnerEligibleLocked(p, t.OwnerID) {
			return p, t, nil
		}
	}
	return p, SessionToken{}, errDeviceClient
}
func (s *Store) IssueProjectTokenFor(actor, project, label string) (string, error) {
	return s.IssueProjectTokenWithOptionsFor(actor, project, label, ProjectTokenOptions{})
}

func (s *Store) IssueProjectTokenWithOptionsFor(actor, project, label string, options ProjectTokenOptions) (string, error) {
	raw, _, err := s.issueProjectTokenGuarded(actor, project, label, options, nil)
	return raw, err
}
func (s *Store) issueProjectTokenGuarded(actor, project, label string, options ProjectTokenOptions, guard commitGuard) (string, projectTokenDTO, error) {
	label = strings.TrimSpace(label)
	if len(label) > 128 || !utf8.ValidString(label) || strings.ContainsAny(label, "\x00\r\n") {
		return "", projectTokenDTO{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := guard.check(); err != nil {
		return "", projectTokenDTO{}, err
	}
	if s.closed {
		return "", projectTokenDTO{}, ErrStorage
	}
	if err := s.expireLocked(); err != nil {
		return "", projectTokenDTO{}, err
	}
	p, ok := s.data.Projects[project]
	if !ok || !p.Enabled || p.Deleting || p.Mode != "token" || !s.tokenOwnerEligibleLocked(p, actor) {
		return "", projectTokenDTO{}, ErrForbidden
	}
	metadata, err := s.projectTokenMetadataLocked(actor, project, options)
	if err != nil {
		return "", projectTokenDTO{}, err
	}
	if activeTokenCount(p.Tokens, s.now()) >= projectTokenCap {
		return "", projectTokenDTO{}, ErrTokenCap
	}
	raw := "asb_p_" + rand.Text() + rand.Text()
	metadata.Kind, metadata.Label, metadata.ID = tokenProject, label, rand.Text()+rand.Text()
	metadata.OwnerID, metadata.TokenHash = actor, digest(raw)
	p.Tokens = append(append([]SessionToken(nil), p.Tokens...), metadata)
	if err := s.commitProject(p); err != nil {
		return "", projectTokenDTO{}, err
	}
	return raw, projectTokenProjection(p, metadata), nil
}
func newUserCode() string {
	const alphabet = "BCDFGHJKLMNPQRSTVWXZ"
	out := make([]byte, 8)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			panic(err)
		}
		out[i] = alphabet[n.Int64()]
	}
	return string(out[:4]) + "-" + string(out[4:])
}
func normalizeUserCode(code string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
}
func (s *Store) BeginDeviceAuthorization(project, raw string, r DeviceRequest) (DeviceResponse, error) {
	if !r.valid() {
		return DeviceResponse{}, errDeviceRequest
	}
	// Display metadata is never part of the persisted request or its replay
	// identity. Only the current process retains it under the issued code hash.
	cwdHint := r.CWDBasename
	r.CWDBasename = ""
	s.mu.Lock()
	defer s.mu.Unlock()
	p, t, err := s.projectTokenLocked(project, raw)
	if err != nil {
		return DeviceResponse{}, err
	}
	now := s.now().UTC()
	previousDevices := p.Devices
	kept := make([]DeviceAuthorization, 0, len(p.Devices)+1)
	for _, d := range p.Devices {
		if !now.Before(d.ExpiresAt) {
			continue
		}
		if d.TokenID == t.ID && d.Request.RequestID == r.RequestID {
			if d.Status != "pending" || d.Request != r {
				return DeviceResponse{}, errDeviceGrant
			}
			continue
		}
		kept = append(kept, d)
	}
	if len(kept) >= 32 {
		return DeviceResponse{}, errDeviceSlow
	}
	code := rand.Text() + rand.Text()
	user := newUserCode()
	// User codes resolve across projects, so avoid even a rare cross-project collision.
	for {
		collision := false
		h := digest(normalizeUserCode(user))
		for _, project := range s.data.Projects {
			for _, d := range project.Devices {
				if d.UserHash == h && now.Before(d.ExpiresAt) {
					collision = true
				}
			}
		}
		if !collision {
			break
		}
		user = newUserCode()
	}
	p.Tokens = touchProjectToken(p.Tokens, t.ID, now)
	p.Devices = append(kept, DeviceAuthorization{ScopeVersion: access.ScopeVersion, Scopes: access.DelegatedScopes(t.Scopes), Request: r, TokenID: t.ID, CodeHash: digest(code), UserHash: digest(normalizeUserCode(user)), CreatedAt: now, ExpiresAt: now.Add(deviceLifetime), Status: "pending"})
	if err = s.commitProject(p); err != nil {
		return DeviceResponse{}, err
	}
	for _, previous := range previousDevices {
		if !now.Before(previous.ExpiresAt) || (previous.TokenID == t.ID && previous.Request.RequestID == r.RequestID) {
			delete(s.deviceDirectoryHints, previous.CodeHash)
		}
	}
	if cwdHint != "" {
		if s.deviceDirectoryHints == nil {
			s.deviceDirectoryHints = map[string]deviceDirectoryHint{}
		}
		s.deviceDirectoryHints[digest(code)] = deviceDirectoryHint{basename: cwdHint, expiresAt: now.Add(deviceLifetime)}
	}
	return DeviceResponse{DeviceCode: code, UserCode: user, VerificationURI: "/auth/code", VerificationURIComplete: "/auth/code?code=" + url.QueryEscape(user), ExpiresIn: 600, Interval: 5}, nil
}
func (s *Store) deviceForUserLocked(actor, code string) (Project, int, SessionToken, error) {
	if s.closed || len(code) > 32 {
		return Project{}, 0, SessionToken{}, ErrForbidden
	}
	h := digest(normalizeUserCode(code))
	for _, p := range s.data.Projects {
		if !p.Enabled || p.Deleting {
			continue
		}
		for i, d := range p.Devices {
			if !equal(d.UserHash, h) || !s.now().Before(d.ExpiresAt) {
				continue
			}
			for _, t := range p.Tokens {
				if t.ID == d.TokenID && t.Kind == tokenProject && !t.Revoked && s.tokenOwnerEligibleLocked(p, t.OwnerID) && s.tokenOwnerEligibleLocked(p, actor) && (actor == t.OwnerID || s.projectRoleLocked(actor, p.Name) == "owner") {
					return p, i, t, nil
				}
			}
		}
	}
	return Project{}, 0, SessionToken{}, ErrForbidden
}
func (s *Store) deviceView(p Project, d DeviceAuthorization, t SessionToken) DeviceView {
	v := DeviceView{ClientID: d.Request.ClientID, CWDBasename: s.deviceDirectoryHints[d.CodeHash].basename, Project: p.Name, OwnerID: t.OwnerID, Harness: d.Request.Harness, SessionID: d.Request.SessionID, InstanceID: d.Request.InstanceID, Status: d.Status, CreatedAt: d.CreatedAt, ExpiresAt: d.ExpiresAt}
	for _, g := range p.Tokens {
		if g.ID == d.GrantID {
			v.Nickname = g.Nickname
		}
	}
	return v
}
func (s *Store) DeviceAuthorizationFor(actor, code string) (DeviceView, error) {
	return s.deviceAuthorizationForGuarded(actor, code, nil)
}
func (s *Store) deviceAuthorizationForGuarded(actor, code string, guard commitGuard) (DeviceView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := guard.check(); err != nil {
		return DeviceView{}, err
	}
	s.pruneDeviceDirectoryHintsLocked(s.now())
	p, i, t, e := s.deviceForUserLocked(actor, code)
	if e != nil {
		return DeviceView{}, e
	}
	return s.deviceView(p, p.Devices[i], t), nil
}
func (s *Store) ApproveDeviceAuthorization(actor, code, nickname string, deny bool) (DeviceView, error) {
	return s.approveDeviceAuthorizationGuarded(actor, code, nickname, deny, nil)
}
func (s *Store) approveDeviceAuthorizationGuarded(actor, code, nickname string, deny bool, guard commitGuard) (DeviceView, error) {
	nickname = strings.TrimSpace(nickname)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := guard.check(); err != nil {
		return DeviceView{}, err
	}
	if err := s.expireLocked(); err != nil {
		return DeviceView{}, err
	}
	p, i, parent, err := s.deviceForUserLocked(actor, code)
	if err != nil {
		return DeviceView{}, err
	}
	d := p.Devices[i]
	if d.Status != "pending" {
		return DeviceView{}, errDeviceGrant
	}
	if !deny {
		if !validNickname(nickname) {
			return DeviceView{}, ErrInvalid
		}
		for _, t := range p.Tokens {
			if tokenReservesName(t, nickname) {
				return DeviceView{}, ErrNicknameTaken
			}
		}
		if activeTokenCount(p.Tokens, s.now()) >= projectTokenCap {
			return DeviceView{}, ErrTokenCap
		}
		now := s.now().UTC()
		id := rand.Text() + rand.Text()
		expiresAt := now.Add(sessionLifetime)
		if !parent.ExpiresAt.IsZero() && parent.ExpiresAt.Before(expiresAt) {
			expiresAt = parent.ExpiresAt
		}
		// Reserve the nickname and bind identity in the same commit as approval.
		p.Tokens = append(append([]SessionToken(nil), p.Tokens...), SessionToken{ScopeVersion: access.ScopeVersion, Scopes: append([]string(nil), d.Scopes...), Kind: tokenSession, ParentID: parent.ID, ID: id, OwnerID: parent.OwnerID, Nickname: nickname, IssuedAt: now, ExpiresAt: expiresAt, BoundIdentity: d.Request.identity(), TokenHash: digest(rand.Text() + rand.Text())})
		for j := range p.Tokens {
			if p.Tokens[j].ID == parent.ID {
				p.Tokens[j].Approvals++
			}
		}
		d.Status = "approved"
		d.GrantID = id
		d.ApprovedBy = actor
	} else {
		d.Status = "denied"
	}
	p.Devices = append([]DeviceAuthorization(nil), p.Devices...)
	p.Devices[i] = d
	if err = s.commitProject(p); err != nil {
		return DeviceView{}, err
	}
	view := s.deviceView(p, d, parent)
	delete(s.deviceDirectoryHints, d.CodeHash)
	return view, nil
}
func (s *Store) ExchangeDeviceToken(project, raw, code, clientID string, plaintext bool) (DeviceTokenResponse, error) {
	if s.beforeTokenExchange != nil {
		s.beforeTokenExchange()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if plaintext && !s.data.AllowHTTP {
		return DeviceTokenResponse{}, errHTTPNotAllowed
	}
	p, parent, err := s.projectTokenLocked(project, raw)
	if err != nil {
		return DeviceTokenResponse{}, err
	}
	if len(code) > 256 || code == "" {
		return DeviceTokenResponse{}, errDeviceGrant
	}
	h := digest(code)
	var found *DeviceAuthorization
	for i := range p.Devices {
		d := &p.Devices[i]
		if equal(d.CodeHash, h) && d.TokenID == parent.ID && d.Request.ClientID == clientID {
			found = d
			break
		}
	}
	if found == nil {
		return DeviceTokenResponse{}, errDeviceGrant
	}
	d := *found
	now := s.now().UTC()
	if !now.Before(d.ExpiresAt) {
		return DeviceTokenResponse{}, errDeviceExpired
	}
	if d.Status == "denied" {
		return DeviceTokenResponse{}, errDeviceDenied
	}
	if s.devicePoll == nil {
		s.devicePoll = map[string]devicePollState{}
	}
	poll := s.devicePoll[h]
	if poll.interval == 0 {
		poll.interval = 5 * time.Second
	}
	if now.Before(poll.next) {
		poll.next = now.Add(poll.interval)
		s.devicePoll[h] = poll
		return DeviceTokenResponse{}, errDeviceSlow
	}
	poll.next = now.Add(poll.interval)
	s.devicePoll[h] = poll
	if d.Status == "pending" {
		return DeviceTokenResponse{}, errDevicePending
	}
	for i, t := range p.Tokens {
		if t.ID != d.GrantID {
			continue
		}
		if !s.sessionTokenEligibleLocked(p, t) || !t.FirstUsedAt.IsZero() {
			return DeviceTokenResponse{}, errDeviceGrant
		}
		raw := "asb_s_" + rand.Text() + rand.Text()
		t.TokenHash = digest(raw)
		p.Tokens = append([]SessionToken(nil), p.Tokens...)
		p.Tokens[i] = t
		for j := range p.Tokens {
			if p.Tokens[j].ID == parent.ID {
				p.Tokens[j].LastUsedAt = now
			}
		}
		if err = s.commitProject(p); err != nil {
			return DeviceTokenResponse{}, err
		}
		return DeviceTokenResponse{AccessToken: raw, TokenType: "Bearer", ExpiresIn: int(t.ExpiresAt.Sub(now).Seconds()), GrantID: t.ID, Nickname: t.Nickname}, nil
	}
	return DeviceTokenResponse{}, errDeviceGrant
}

// ShortSessionID keeps the review readable; the full value remains in title.
func (v DeviceView) ShortSessionID() string {
	if len(v.SessionID) > 12 {
		return v.SessionID[:12] + "…"
	}
	return v.SessionID
}

// The code itself is never retained. Expiry is pruned both on lookup and by
// the store's existing periodic sweep, even if no new request is issued.
type deviceDirectoryHint struct {
	basename  string
	expiresAt time.Time
}

func (s *Store) pruneDeviceDirectoryHintsLocked(now time.Time) {
	for hash, hint := range s.deviceDirectoryHints {
		if !now.Before(hint.expiresAt) {
			delete(s.deviceDirectoryHints, hash)
		}
	}
}

// Called after durable publication, with Store.mu held; retained codes keep their rate state.
func (s *Store) pruneDevicePollLocked() {
	if len(s.devicePoll) == 0 {
		return
	}
	live := make(map[string]bool)
	for _, project := range s.data.Projects {
		for _, device := range project.Devices {
			live[device.CodeHash] = true
		}
	}
	for hash := range s.devicePoll {
		if !live[hash] {
			delete(s.devicePoll, hash)
		}
	}
}
