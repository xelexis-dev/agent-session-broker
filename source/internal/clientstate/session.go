package clientstate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Device authorization outcomes kept with a session so a restarted client
// resumes waiting, and never re-requests a code after a final answer without
// an explicit user request.
const (
	// DeviceRequested records a request id before its code request is sent,
	// so a lost answer or a restart retries with the same id.
	DeviceRequested = "requested"
	DevicePending   = "pending"
	DeviceRedeemed  = "redeemed"
	DeviceDenied    = "denied"
	DeviceExpired   = "expired"
	DeviceRejected  = "rejected"
	DeviceFailed    = "failed"
)

// SessionCredential is the broker-issued per-session bearer credential. It is
// the only value sent to /mcp; the project token never reaches that endpoint.
type SessionCredential struct {
	Scopes         []string  `json:"scopes,omitempty"`
	AdmissionMode  string    `json:"admission_mode,omitempty"`
	AccessToken    string    `json:"access_token"`
	TokenType      string    `json:"token_type"`
	IssuedAt       time.Time `json:"issued_at,omitzero"`
	ExpiresAt      time.Time `json:"expires_at,omitzero"`
	SessionGrantID string    `json:"session_grant_id,omitempty"`
	Nickname       string    `json:"nickname,omitempty"`
}

// DeviceAuthorization is the pending or finished code request for a session.
type DeviceAuthorization struct {
	VerificationURIComplete string    `json:"verification_uri_complete,omitempty"`
	RequestedNickname       string    `json:"requested_nickname,omitempty"`
	RequestID               string    `json:"request_id"`
	DeviceCode              string    `json:"device_code,omitempty"`
	UserCode                string    `json:"user_code,omitempty"`
	VerificationURI         string    `json:"verification_uri,omitempty"`
	ExpiresAt               time.Time `json:"expires_at,omitzero"`
	Interval                int       `json:"interval,omitempty"`
	Status                  string    `json:"status"`
	Error                   string    `json:"error,omitempty"`
}

type DirectAuthorization struct {
	Reattach          bool      `json:"reattach,omitempty"`
	RequestID         string    `json:"request_id"`
	RequestedNickname string    `json:"requested_nickname,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	Status            string    `json:"status"`
	Restricted        bool      `json:"restricted,omitempty"`
}

// SessionState is one conversation's durable state under the auth directory:
// registration ownership (resume secret, revision, replayable request),
// the session credential and the device authorization progress.
// AutoRecovery records automatic recovery without persisting a bearer in
// diagnostics. Retry deadlines survive restart, including Retry-After.
type AutoRecovery struct {
	Kind          string    `json:"kind,omitempty"`
	StopReason    string    `json:"stop_reason,omitempty"`
	Attempts      int       `json:"attempts,omitempty"`
	NextAttemptAt time.Time `json:"next_attempt_at,omitzero"`
}

type SessionState struct {
	Handover             *HandoverAttempt     `json:"handover,omitempty"`
	AdmissionFingerprint string               `json:"admission_fingerprint,omitempty"`
	RestrictedAdmission  bool                 `json:"restricted_admission,omitempty"`
	LastAdmissionMode    string               `json:"last_admission_mode,omitempty"`
	AutoRecovery         *AutoRecovery        `json:"auto_recovery,omitempty"`
	SchemaVersion        int                  `json:"schema_version"`
	Scope                string               `json:"scope"`
	Project              string               `json:"project"`
	Harness              string               `json:"harness"`
	SessionID            string               `json:"session_id"`
	ClientID             string               `json:"client_id"`
	Registration         Registration         `json:"registration"`
	Credential           *SessionCredential   `json:"credential,omitempty"`
	Direct               *DirectAuthorization `json:"direct,omitempty"`
	Device               *DeviceAuthorization `json:"device,omitempty"`
}

const sessionSchemaVersion = 1

// SessionKey names one conversation's state directory. Unlike RegistrationKey
// it includes the PC client id, so a copied auth directory on another PC does
// not silently share a session's ownership proof.
func SessionKey(scope, harness, sessionID, clientID string) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + harness + "\x00" + sessionID + "\x00" + clientID))
	return hex.EncodeToString(sum[:16])
}

func (s *Store) sessionDir(key string) string {
	return filepath.Join(s.dir, "sessions", key)
}

func (s *Store) LoadSession(key string) (SessionState, bool, error) {
	return s.loadSession(key, true)
}

func (s *Store) loadSession(key string, touch bool) (SessionState, bool, error) {
	data, err := os.ReadFile(filepath.Join(s.sessionDir(key), "conn.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return SessionState{}, false, nil
	}
	if err != nil {
		return SessionState{}, false, errors.New("session state is unreadable")
	}
	var state SessionState
	if json.Unmarshal(data, &state) != nil || state.SchemaVersion != sessionSchemaVersion || state.Registration.Secret == "" {
		return SessionState{}, false, errors.New("session state is corrupt; remove it and authorize again")
	}
	state.Registration.HandedOverTo = s.handedOverTo(key)
	// Freshness only feeds pruning; a missed touch at worst makes state look older.
	if touch {
		_ = os.Chtimes(filepath.Join(s.sessionDir(key), "conn.json"), time.Now(), time.Now())
	}
	return state, true, nil
}

func (s *Store) SaveSession(key string, state SessionState) error {
	if state.Registration.Secret == "" {
		return errors.New("session state requires a resume secret")
	}
	state.SchemaVersion = sessionSchemaVersion
	data, err := json.MarshalIndent(state, "", " ")
	if err != nil {
		return err
	}
	dir := s.sessionDir(key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.New("session state directory could not be created")
	}
	if err := writeFileWith(filepath.Join(dir, "conn.json"), data, replaceFile); err != nil {
		return errors.New("session state could not be written")
	}
	return nil
}

// LockSession is the per-conversation OS lock for the new state layout. It is
// held for the binding's lifetime like Lock is for legacy registrations.
func (s *Store) LockSession(key string) (func(), error) {
	dir := s.sessionDir(key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.New("session state directory could not be created")
	}
	return lockFile(filepath.Join(dir, "conn.lock"))
}

// AdoptLegacyRegistration copies the ownership proof of a legacy registration
// file into a new session state when none exists yet. The legacy file stays
// untouched, and a legacy client that still holds its lock blocks adoption:
// the broker would otherwise see two owners of one registration.
func (s *Store) AdoptLegacyRegistration(key, legacyKey string) (bool, error) {
	if _, found, err := s.LoadSession(key); err != nil || found {
		return false, err
	}
	legacy, found, err := s.LoadRegistration(legacyKey)
	if err != nil || !found {
		return false, err
	}
	release, err := s.Lock(legacyKey)
	if err != nil {
		return false, err
	}
	defer release()
	state := SessionState{
		Project: legacy.Project, Harness: legacy.Harness, SessionID: legacy.SessionID,
		Registration: legacy,
	}
	if err := s.SaveSession(key, state); err != nil {
		return false, err
	}
	return true, nil
}
