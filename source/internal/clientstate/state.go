// Package clientstate keeps the local MCP client's durable registration state:
// a stable per-PC client id and, per (project, harness, session), the
// registration id, assigned name and client-generated resume secret. Nothing
// here is exposed to the model; the secret proves ownership to the broker.
package clientstate

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const EnvDir = "AGENT_SESSION_BROKER_STATE_DIR"

var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)

type Registration struct {
	ParentPID      int                    `json:"parent_pid,omitempty"`
	ProcessKey     string                 `json:"process_key,omitempty"`
	SavedAt        time.Time              `json:"saved_at,omitzero"`
	HandedOverTo   string                 `json:"handed_over_to,omitempty"`
	HandoverFrom   *protocol.HandoverFrom `json:"handoverFrom,omitempty"`
	Detached       bool                   `json:"detached,omitempty"`
	InitialAttempt string                 `json:"initial_registration_attempt,omitempty"`
	RegistrationID string                 `json:"registration_id,omitempty"`
	Name           string                 `json:"name,omitempty"`
	Secret         string                 `json:"secret"`
	Revision       uint64                 `json:"revision,omitempty"`
	Project        string                 `json:"project"`
	Harness        string                 `json:"harness"`
	SessionID      string                 `json:"session_id"`
	// Pending fields describe a register request whose answer may have been
	// lost. Retrying the exact same request id, lease and revision lets the
	// broker answer idempotently instead of creating or fencing anew.
	PendingRequestID string `json:"pending_request_id,omitempty"`
	PendingLease     string `json:"pending_lease,omitempty"`
	PendingRevision  uint64 `json:"pending_revision,omitempty"`
}

var clientIDMu sync.Mutex

type Store struct {
	dir               string
	steerLockObserved func() // Test barrier after the lock inode snapshot.
	// Guarded by diagnosticsMu; only successful renewal summaries are deferred.
	diagnosticRenewals     map[string]diagnosticRenewal
	deviceDiagnosticCauses map[string]string
}

// DefaultDir is the per-user state directory, overridable for tests and
// unusual profiles through EnvDir.
func DefaultDir() (string, error) {
	if dir := os.Getenv(EnvDir); dir != "" {
		if !filepath.IsAbs(dir) {
			return "", errors.New("client state directory override must be absolute")
		}
		return dir, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", errors.New("client state directory is unavailable")
	}
	return filepath.Join(base, "agent-session-broker"), nil
}

func Open(dir string) (*Store, error) {
	if dir == "" {
		resolved, err := DefaultDir()
		if err != nil {
			return nil, err
		}
		dir = resolved
	}
	if !filepath.IsAbs(dir) {
		return nil, errors.New("client state directory must be absolute")
	}
	if err := os.MkdirAll(filepath.Join(dir, "registrations"), 0o700); err != nil {
		return nil, errors.New("client state directory could not be created")
	}
	return &Store{dir: dir}, nil
}

func (s *Store) Dir() string { return s.dir }

// ClientID returns the stable per-PC identity, creating it on first use. Two
// clients starting at once on the same PC serialize on an OS lock so both
// observe the single identity.
func (s *Store) ClientID() (string, error) {
	clientIDMu.Lock()
	defer clientIDMu.Unlock()
	release, err := lockFileWait(filepath.Join(s.dir, "client-id.lock"), 5*time.Second)
	if err != nil {
		return "", errors.New("client id lock is unavailable")
	}
	defer release()
	path := filepath.Join(s.dir, "client-id")
	data, err := os.ReadFile(path)
	if err == nil {
		id := strings.TrimSpace(string(data))
		if clientIDPattern.MatchString(id) {
			return id, nil
		}
		return "", errors.New("client id file is invalid; remove it to generate a new identity")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", errors.New("client id file is unreadable")
	}
	id := "pc-" + strings.ToLower(rand.Text())
	if err := writeFile(path, []byte(id+"\n")); err != nil {
		return "", errors.New("client id file could not be written")
	}
	return id, nil
}

func lockFileWait(path string, timeout time.Duration) (func(), error) {
	deadline := time.Now().Add(timeout)
	for {
		release, err := lockFile(path)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, ErrLocked) || time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// NewSecret returns a 64-character hex resume secret from 32 random bytes.
func NewSecret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// RegistrationKey names the state file for one registration. scope should
// identify the broker endpoint and project so the same session registered
// with two brokers keeps separate secrets and locks.
func RegistrationKey(scope, harness, sessionID string) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + harness + "\x00" + sessionID))
	return hex.EncodeToString(sum[:16])
}

func (s *Store) registrationPath(key string) string {
	return filepath.Join(s.dir, "registrations", key+".json")
}

func (s *Store) LoadRegistration(key string) (Registration, bool, error) {
	data, err := os.ReadFile(s.registrationPath(key))
	if errors.Is(err, fs.ErrNotExist) {
		return Registration{}, false, nil
	}
	if err != nil {
		return Registration{}, false, errors.New("registration state is unreadable")
	}
	var r Registration
	// A pending-only record (secret plus the unanswered first request, no
	// registration id yet) is valid: it is what a restarted client replays.
	if json.Unmarshal(data, &r) != nil || r.Secret == "" || (r.RegistrationID == "" && r.PendingRequestID == "" && r.InitialAttempt != "pending" && r.InitialAttempt != "stopped") {
		return Registration{}, false, errors.New("registration state is corrupt; remove it and register again")
	}
	// Freshness only feeds pruning; a missed touch at worst makes state look older.
	_ = os.Chtimes(s.registrationPath(key), time.Now(), time.Now())
	return r, true, nil
}

func (s *Store) SaveRegistration(key string, r Registration) error {
	data, err := json.MarshalIndent(r, "", " ")
	if err != nil {
		return err
	}
	if err := writeFile(s.registrationPath(key), data); err != nil {
		return errors.New("registration state could not be written")
	}
	return nil
}

func (s *Store) RemoveRegistration(key string) error {
	err := os.Remove(s.registrationPath(key))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errors.New("registration state could not be removed")
	}
	return nil
}

// Lock takes the OS-level exclusive lock for one registration so a second
// client process on the same PC cannot run a competing receiver. The lock is
// released by the returned function or by process exit.
func (s *Store) Lock(key string) (func(), error) {
	return lockFile(filepath.Join(s.dir, "registrations", key+".lock"))
}

var renameFile = os.Rename

func writeFile(path string, data []byte) error {
	return writeFileWith(path, data, renameFile)
}

// Windows handles without FILE_SHARE_DELETE (ReadFile/Chtimes, clients, scanners) can briefly block replacement.
func replaceFile(from, to string) error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := renameFile(from, to)
		if err == nil || !replaceBlocked(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writeFileWith(path string, data []byte, rename func(string, string) error) (err error) {
	temporary := path + ".tmp-" + strings.ToLower(rand.Text())
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(temporary)
		}
	}()
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return rename(temporary, path)
}

// MACAddress reports the first hardware address of a non-loopback interface
// in index order, as registration metadata only. It is empty when no
// interface exposes one.
func MACAddress() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	sort.Slice(interfaces, func(i, j int) bool { return interfaces[i].Index < interfaces[j].Index })
	for _, i := range interfaces {
		if i.Flags&net.FlagLoopback != 0 || len(i.HardwareAddr) == 0 {
			continue
		}
		return i.HardwareAddr.String()
	}
	return ""
}
