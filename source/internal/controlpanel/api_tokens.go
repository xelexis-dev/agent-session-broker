package controlpanel

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	errAPIUnauthorized = errors.New("invalid_token")
	errAPICredentials  = errors.New("invalid_credentials")
	errAPIScope        = errors.New("insufficient_scope")
	errAPITokenCap     = errors.New("token_cap_reached")
	errAPINotFound     = errors.New("not_found")
	errAPIJSON         = errors.New("invalid_json")
	errAPIUnknownField = errors.New("unknown_field")
	errAPIMedia        = errors.New("unsupported_media_type")
	errAPILarge        = errors.New("payload_too_large")
	errAPIDisabled     = errors.New("api_disabled")
	errAPIBusy         = errors.New("busy")
	errAPIRate         = errors.New("rate_limited")
)

type userAPIToken struct {
	V          int       `json:"v"`
	ID         string    `json:"id"`
	Hash       string    `json:"hash"`
	User       string    `json:"user"`
	Rev        uint64    `json:"rev"`
	Label      string    `json:"label"`
	Scopes     []string  `json:"scopes"`
	Source     string    `json:"source"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

// UserTokenView never contains a digest or password verifier.
type UserTokenView struct {
	ID         string    `json:"id"`
	Label      string    `json:"label"`
	Scopes     []string  `json:"scopes"`
	Source     string    `json:"source"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

func (t userAPIToken) view() UserTokenView {
	return UserTokenView{t.ID, t.Label, append([]string(nil), t.Scopes...), t.Source, t.CreatedAt, t.ExpiresAt, t.LastUsedAt}
}
func (t userAPIToken) key() string { return "user-tokens/" + t.User + "/" + t.ID }

type apiPrincipal struct {
	userID, tokenID string
	revision        uint64
	scopes          []string
	plaintext       bool
}

func (a apiPrincipal) guardLocked(s *Store, scope string) commitGuard {
	return func() error {
		if s.closed {
			return ErrStorage
		}
		if a.plaintext && !s.data.AllowHTTP {
			return errHTTPNotAllowed
		}
		if !s.api.enabled {
			return errAPIDisabled
		}
		t, ok := s.api.tokens[a.tokenID]
		u, active := s.activeUserLocked(a.userID)
		if !ok || !active || t.User != a.userID || t.Rev != u.Revision || a.revision != u.Revision || !s.now().Before(t.ExpiresAt) {
			return errAPIUnauthorized
		}
		if scope != "" && !containsAPIScope(t.Scopes, scope) {
			return errAPIScope
		}
		if scope == "admin" && u.Role != "admin" {
			return ErrForbidden
		}
		return nil
	}
}
func containsAPIScope(scopes []string, scope string) bool {
	for _, v := range scopes {
		if v == scope {
			return true
		}
	}
	return false
}
func validAPIScopes(scopes []string, admin bool) bool {
	if len(scopes) == 0 || len(scopes) > 4 {
		return false
	}
	seen := map[string]bool{}
	for _, v := range scopes {
		if seen[v] || (v != "read" && v != "write" && v != "approve" && v != "admin") || v == "admin" && !admin {
			return false
		}
		seen[v] = true
	}
	return true
}

type apiRate struct {
	until time.Time
	n     int
}
type apiAuditEvent struct {
	At     time.Time `json:"at"`
	Action string    `json:"action"`
	Actor  string    `json:"actor,omitempty"`
	Target string    `json:"target,omitempty"`
	Role   string    `json:"role,omitempty"`
	Result string    `json:"result"`
	Count  int       `json:"count,omitempty"`
}
type apiState struct {
	lastSetting                *adminAPISetting
	lastFlush                  time.Time
	enabled, dirtySwitch       bool
	tokens                     map[string]userAPIToken
	rows                       map[string]bool
	pendingDeletes             map[string]bool
	audits                     map[string]apiAuditEvent
	malformed                  int
	loginPeers, writes, reauth map[string]apiRate
	global                     apiRate
	refusals                   map[string]int
}

func newAPIState() apiState {
	return apiState{tokens: map[string]userAPIToken{}, rows: map[string]bool{}, pendingDeletes: map[string]bool{}, audits: map[string]apiAuditEvent{}, loginPeers: map[string]apiRate{}, writes: map[string]apiRate{}, reauth: map[string]apiRate{}, refusals: map[string]int{}}
}
func (s *Store) loadAPI(records map[string][]byte) {
	s.api = newAPIState()
	for key, raw := range records {
		switch {
		case key == "settings/admin-api":
			var v adminAPISetting
			if strictAPIBytes(raw, &v) == nil && v.V == 1 && (v.At.IsZero() || usernamePattern.MatchString(v.Actor) && v.New == v.Enabled && v.Old != v.New) {
				s.api.enabled = v.Enabled
				if !v.At.IsZero() {
					s.api.lastSetting = &v
				}
			}
		case strings.HasPrefix(key, "user-tokens/"):
			s.api.rows[key] = true
			var t userAPIToken
			if strictAPIBytes(raw, &t) != nil || !validAPIToken(t, key) || len(s.api.tokens) >= 2048 {
				s.api.malformed++
				continue
			}
			s.api.tokens[t.ID] = t
		case strings.HasPrefix(key, "api-audit/"):
			var e apiAuditEvent
			if len(raw) <= 1024 && json.Unmarshal(raw, &e) == nil && !e.At.IsZero() {
				s.api.audits[key] = e
			}
		}
	}
}

var apiTokenIDPattern = regexp.MustCompile(`^[A-Z2-7]{52}$`)

func validAPIToken(t userAPIToken, key string) bool {
	h, e := hex.DecodeString(t.Hash)
	return t.V == 1 && apiTokenIDPattern.MatchString(t.ID) && usernamePattern.MatchString(t.User) && t.key() == key && e == nil && len(h) == 32 && t.Rev > 0 && len(t.Label) <= 128 && validProfile(t.Label, "") && validAPIScopes(t.Scopes, true) && (t.Source == "login" || t.Source == "panel") && !t.CreatedAt.IsZero() && t.ExpiresAt.Sub(t.CreatedAt) >= 300*time.Second && t.ExpiresAt.Sub(t.CreatedAt) <= 90*24*time.Hour
}
func (s *Store) AdminAPIEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.closed && s.api.enabled
}
func (s *Store) SetAdminAPI(actor string, enabled bool, version ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isAdminLocked(actor) {
		return ErrForbidden
	}
	if err := checkSettingsVersion(version, policySettingsVersion("admin-api", s.api.enabled)); err != nil {
		return err
	}
	if s.api.enabled == enabled && !s.api.dirtySwitch {
		return nil
	}
	setting := &adminAPISetting{V: 1, Enabled: enabled, Actor: actor, At: s.now().UTC(), Old: s.api.enabled, New: enabled}
	if s.api.enabled == enabled && s.api.lastSetting != nil {
		copy := *s.api.lastSetting
		setting = &copy
	}
	if !enabled {
		s.api.enabled = false
		s.api.lastSetting = setting
		s.api.dirtySwitch = true
	}
	raw, _ := json.Marshal(setting)
	if err := s.apiBatchLocked(map[string][]byte{"settings/admin-api": raw}, nil, apiAuditEvent{Action: "api_switch", Actor: actor, Result: "success"}); err != nil {
		return err
	}
	s.api.enabled = enabled
	s.api.lastSetting = setting
	s.api.dirtySwitch = false
	return nil
}
func (s *Store) apiBatchLocked(puts map[string][]byte, deletes []string, events ...apiAuditEvent) error {
	if s.closed || s.writeBatch == nil {
		return ErrStorage
	}
	next := maps.Clone(s.api.audits)
	for _, event := range events {
		if event.Action != "" {
			event.At = s.now().UTC()
			key := fmt.Sprintf("api-audit/%020d-%s", event.At.UnixNano(), rand.Text())
			raw, _ := json.Marshal(event)
			puts[key] = raw
			next[key] = event
		}
	}
	// Successes and aggregate refusals have independent durable caps.
	for _, aggregate := range []bool{false, true} {
		keys := []string{}
		for key, e := range next {
			if (e.Count > 0) == aggregate {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		cap := 4096
		if aggregate {
			cap = 1024
		}
		for len(keys) > cap {
			key := keys[0]
			keys = keys[1:]
			delete(next, key)
			delete(puts, key)
			deletes = append(deletes, key)
		}
	}
	if s.writeBatch(puts, deletes) != nil {
		return ErrStorage
	}
	s.api.audits = next
	return nil
}
func (s *Store) sweepAPILocked() error {
	for id, t := range s.api.tokens {
		u, ok := s.data.Users[t.User]
		if !ok || !u.Enabled || u.Revision != t.Rev || !s.now().Before(t.ExpiresAt) {
			delete(s.api.tokens, id)
			s.api.pendingDeletes[t.key()] = true
		}
	}
	puts := map[string][]byte{}
	deletes := []string{}
	for key := range s.api.pendingDeletes {
		deletes = append(deletes, key)
	}
	if s.api.dirtySwitch {
		puts["settings/admin-api"] = s.adminAPISettingBytesLocked()
	}

	if len(puts)+len(deletes) > 0 {
		if s.apiBatchLocked(puts, deletes, apiAuditEvent{}) != nil {
			return ErrStorage
		}
		for _, key := range deletes {
			delete(s.api.rows, key)
			delete(s.api.pendingDeletes, key)
		}
		s.api.dirtySwitch = false
	}
	if !s.api.lastFlush.IsZero() && s.now().Sub(s.api.lastFlush) < time.Minute {
		return nil
	}
	pending := []apiAuditEvent{}
	for key, count := range s.api.refusals {
		pending = append(pending, apiAuditEvent{Action: key, Result: "refused", Count: count})
	}
	if len(pending) > 0 {
		if s.apiBatchLocked(map[string][]byte{}, nil, pending...) != nil {
			return ErrStorage
		}
		clear(s.api.refusals)
	}
	s.api.lastFlush = s.now()
	return nil
}
func rateAllowed(m map[string]apiRate, key string, now time.Time, limit int, period time.Duration) bool {
	for k, v := range m {
		if !now.Before(v.until) {
			delete(m, k)
		}
	}
	v := m[key]
	if v.until.IsZero() {
		if len(m) >= 1024 {
			return true
		}
		v.until = now.Add(period)
	}
	if v.n >= limit {
		return false
	}
	v.n++
	m[key] = v
	return true
}
func (s *Store) apiLoginAllowedLocked(peer string) bool {
	now := s.now()
	if !now.Before(s.api.global.until) {
		s.api.global = apiRate{until: now.Add(time.Minute)}
	}
	if s.api.global.n >= 10 {
		return false
	}
	s.api.global.n++
	return rateAllowed(s.api.loginPeers, peer, now, 5, time.Minute)
}
func (s *Store) apiRefusalLocked(action string) { // Fixed action keys only, never input strings.
	if len(s.api.refusals) < 1024 || s.api.refusals[action] > 0 {
		s.api.refusals[action]++
	}
}
func verifyAPISecret(v passwordVerifier, password string) bool {
	salt, _ := hex.DecodeString(v.Salt)
	expected, _ := hex.DecodeString(v.Hash)
	actual := derivePasswordKey([]byte(password), salt)
	return subtle.ConstantTimeCompare(expected, actual) == 1
}

var dummyAPIVerifier = passwordVerifier{Salt: strings.Repeat("00", 16), Hash: strings.Repeat("00", 32)}

// Panel issuance uses its authenticated session; login verifies the password outside
// Store.mu. Both recheck revision and transport policy at the commit boundary.
func (s *Store) issueUserToken(user, password, label string, seconds int64, scopes []string, source, peer string, plaintext bool, expectedRevision uint64, admit ...func() bool) (string, UserTokenView, error) {
	if len(password) > 1024 || len(user) > 64 {
		return "", UserTokenView{}, ErrInvalid
	}
	s.mu.Lock()
	if source != "panel" && !s.api.enabled {
		s.mu.Unlock()
		return "", UserTokenView{}, errAPIDisabled
	}
	if plaintext && !s.data.AllowHTTP {
		s.mu.Unlock()
		return "", UserTokenView{}, errHTTPNotAllowed
	}
	if source == "login" {
		select {
		case s.hashSlot <- struct{}{}:
		default:
			s.mu.Unlock()
			return "", UserTokenView{}, errAPIBusy
		}
		defer func() { <-s.hashSlot }()
		if !s.apiLoginAllowedLocked(peer) {
			s.mu.Unlock()
			return "", UserTokenView{}, errAPIRate
		}
	}
	u, ok := s.activeUserLocked(user)
	if source == "panel" && (!ok || u.Revision != expectedRevision) {
		s.mu.Unlock()
		return "", UserTokenView{}, ErrForbidden
	}
	v := u.Password
	if !ok {
		v = dummyAPIVerifier
	}
	s.mu.Unlock()
	if len(admit) > 0 && admit[0] != nil && !admit[0]() {
		return "", UserTokenView{}, errAPIRate
	}
	verified := source == "panel" || verifyAPISecret(v, password)
	if s.beforeTokenExchange != nil {
		s.beforeTokenExchange()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, active := s.activeUserLocked(user)
	if source == "panel" && (!active || current.Revision != expectedRevision) {
		return "", UserTokenView{}, ErrForbidden
	}
	if !verified || !ok || !active || u.Revision != current.Revision {
		s.apiRefusalLocked("login_failed")
		return "", UserTokenView{}, errAPICredentials
	}
	if source != "panel" && !s.api.enabled {
		return "", UserTokenView{}, errAPIDisabled
	}
	if plaintext && !s.data.AllowHTTP {
		return "", UserTokenView{}, errHTTPNotAllowed
	}
	if len(password) > 1024 || len(label) > 128 || !validProfile(label, "") {
		return "", UserTokenView{}, ErrInvalid
	}
	if seconds == 0 {
		seconds = 8 * 3600
		if source == "panel" {
			seconds = 30 * 24 * 3600
		}
	}
	if seconds < 300 || seconds > 90*24*3600 {
		return "", UserTokenView{}, ErrInvalid
	}
	if len(scopes) == 0 {
		scopes = []string{"read", "write"}
	}
	if containsAPIScope(scopes, "admin") && u.Role != "admin" {
		return "", UserTokenView{}, ErrForbidden
	}
	if !validAPIScopes(scopes, u.Role == "admin") {
		return "", UserTokenView{}, ErrInvalid
	}
	if err := s.sweepAPILocked(); err != nil {
		return "", UserTokenView{}, err
	}
	count := 0
	for _, t := range s.api.tokens {
		if t.User == user {
			count++
		}
	}
	if count >= 32 || len(s.api.tokens) >= 1024 || len(s.api.rows) >= 2048 {
		return "", UserTokenView{}, errAPITokenCap
	}
	raw := "asb_u_" + rand.Text() + rand.Text()
	now := s.now().UTC()
	t := userAPIToken{1, rand.Text() + rand.Text(), digest(raw), user, u.Revision, label, append([]string(nil), scopes...), source, now, now.Add(time.Duration(seconds) * time.Second), time.Time{}}
	encoded, _ := json.Marshal(t)
	if err := s.apiBatchLocked(map[string][]byte{t.key(): encoded}, nil, apiAuditEvent{Action: "token_issue_" + source, Actor: user, Result: "success"}); err != nil {
		return "", UserTokenView{}, err
	}
	s.api.tokens[t.ID] = t
	s.api.rows[t.key()] = true
	return raw, t.view(), nil
}

// IssueOwnUserToken binds panel issuance to the authenticated session revision.
func (s *Store) IssueOwnUserToken(actor string, revision uint64, label string, seconds int64, scopes []string, principalKey string, plaintext bool, admit ...func() bool) (string, UserTokenView, error) {
	return s.issueUserToken(actor, "", label, seconds, scopes, "panel", principalKey, plaintext, revision, admit...)
}
func (s *Store) ListOwnUserTokens(actor string) ([]UserTokenView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.activeUserLocked(actor); !ok {
		return nil, ErrForbidden
	}
	return s.userTokenViewsLocked(actor), nil
}
func (s *Store) userTokenViewsLocked(user string) []UserTokenView {
	out := []UserTokenView{}
	for _, t := range s.api.tokens {
		u, ok := s.activeUserLocked(user)
		if t.User == user && ok && t.Rev == u.Revision && s.now().Before(t.ExpiresAt) {
			out = append(out, t.view())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (s *Store) revokeUserTokensLocked(actor, id string, all bool, auditActor ...string) error {
	keys := []string{}
	if all {
		for tokenID, t := range s.api.tokens {
			if t.User == actor {
				s.api.pendingDeletes[t.key()] = true
				delete(s.api.tokens, tokenID)
			}
		}
		if s.listRecords == nil {
			return ErrStorage
		}
		rows, err := s.listRecords()
		if err != nil {
			return ErrStorage
		}
		prefix := "user-tokens/" + actor + "/"
		for key := range rows {
			if strings.HasPrefix(key, prefix) {
				keys = append(keys, key)
			}
		}
	} else {
		t, ok := s.api.tokens[id]
		if !ok || t.User != actor {
			return errAPINotFound
		}
		keys = append(keys, t.key())
	}
	for _, key := range keys {
		s.api.pendingDeletes[key] = true
		for id, t := range s.api.tokens {
			if t.key() == key {
				delete(s.api.tokens, id)
			}
		}
	}
	by := actor
	if len(auditActor) == 1 {
		by = auditActor[0]
	}
	if s.apiBatchLocked(map[string][]byte{}, keys, apiAuditEvent{Action: "token_revoke", Actor: by, Target: actor, Result: "success"}) != nil {
		return ErrStorage
	}
	for _, key := range keys {
		delete(s.api.pendingDeletes, key)
		delete(s.api.rows, key)
	}
	return nil
}
func (s *Store) RevokeOwnUserTokens(actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.activeUserLocked(actor); !ok {
		return ErrForbidden
	}
	return s.revokeUserTokensLocked(actor, "", true)
}
func (s *Store) authenticateAPI(raw string) (apiPrincipal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.api.enabled {
		return apiPrincipal{}, errAPIDisabled
	}
	if !strings.HasPrefix(raw, "asb_u_") || len(raw) != 58 {
		return apiPrincipal{}, errAPIUnauthorized
	}
	hash := digest(raw)
	for id, t := range s.api.tokens {
		if t.Hash == hash {
			a := apiPrincipal{userID: t.User, tokenID: id, revision: t.Rev, scopes: append([]string(nil), t.Scopes...)}
			if err := a.guardLocked(s, "")(); err != nil {
				return apiPrincipal{}, err
			}
			if t.LastUsedAt.IsZero() || s.now().Sub(t.LastUsedAt) >= time.Hour {
				t.LastUsedAt = s.now().UTC()
				encoded, _ := json.Marshal(t)
				if s.writeBatch(map[string][]byte{t.key(): encoded}, nil) == nil {
					s.api.tokens[id] = t
				}
			}
			return a, nil
		}
	}
	return apiPrincipal{}, errAPIUnauthorized
}

// apiCommitLocked keeps state and API audit in the same SQLite transaction.
func (s *Store) apiCommitLocked(candidate state, event apiAuditEvent) error {
	return s.commitAuditedAPI(candidate, nil, &event)
}

// strictAPIBytes is also used for durable rows: unknown versions never authenticate.
func strictAPIBytes(raw []byte, target any) error { return decodeAPIJSON(bytes.NewReader(raw), target) }
