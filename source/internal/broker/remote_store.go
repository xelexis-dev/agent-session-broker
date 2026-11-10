package broker

import (
	"encoding/json"
	"errors"
	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const registrationRecordVersion = 1

var registrationIDPattern = regexp.MustCompile(`^[A-Z2-7]{26}$`)
var secretHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// probeRecord is the server-side status check issued to a missing registration.
// It is never transported by itself; the owning client receives it on resume or
// lease renewal and answers without model involvement.
type probeRecord struct {
	ID       string    `json:"id"`
	IssuedAt time.Time `json:"issued_at"`
	Deadline time.Time `json:"deadline"`
	Revision uint64    `json:"revision"`
}

// registrationRecord is the durable name→identity binding. Liveness is never
// persisted: an enabled record restores as missing until its owner presents a
// fresh lease with the resume secret. Suspension is independent management
// policy and survives restoration. The secret itself is stored only as a hash.
type registrationRecord struct {
	HandoverFrom      *protocol.HandoverFrom    `json:"handover_from,omitempty"`
	LastManagement    *protocol.ManagementEvent `json:"last_management,omitempty"`
	AdmissionGrantID  string                    `json:"admission_grant_id,omitempty"`
	AdmissionParentID string                    `json:"admission_parent_id,omitempty"`
	AdmissionMode     string                    `json:"admission_mode,omitempty"`
	OwnerID           string                    `json:"owner_id,omitempty"`
	Version           int                       `json:"version"`
	RegistrationID    string                    `json:"registration_id"`
	Harness           string                    `json:"harness"`
	SessionID         string                    `json:"session_id"`
	ClientID          string                    `json:"client_id"`
	Name              string                    `json:"name"`
	RequestedName     string                    `json:"requested_name,omitempty"`
	SecretHash        string                    `json:"secret_hash"`
	Revision          uint64                    `json:"revision"`
	LeaseGeneration   uint64                    `json:"lease_generation"`
	CreatedAt         time.Time                 `json:"created_at"`
	LastSeen          time.Time                 `json:"last_seen"`
	Detached          bool                      `json:"detached,omitempty"`
	Suspended         bool                      `json:"suspended,omitempty"`
	DetachedAt        time.Time                 `json:"detached_at,omitzero"`
	PID               int                       `json:"pid,omitempty"`
	CWD               string                    `json:"cwd,omitempty"`
	ProcessKey        string                    `json:"process_key,omitempty"`
	MAC               string                    `json:"mac,omitempty"`
	Probe             *probeRecord              `json:"probe,omitempty"`
	// LastRequestID and LastRequestRevision identify the last applied
	// register/resume so an exact retry is answered without a second change.
	LastRequestID       string `json:"last_request_id,omitempty"`
	LastRequestRevision uint64 `json:"last_request_revision,omitempty"`

	// Fence the last receiver disabled by suspension across server restarts.
	SuspendedLeaseID string `json:"suspended_lease_id,omitempty"`

	ReattachAfterCleanup bool `json:"reattach_after_cleanup,omitempty"`
}

func (r *registrationRecord) valid() bool {
	return (r.AdmissionMode == "" || r.AdmissionMode == "manual" || (r.AdmissionMode == "auto" && len(r.AdmissionParentID) == 52 && len(r.AdmissionGrantID) == 52)) && r.Version == registrationRecordVersion && registrationIDPattern.MatchString(r.RegistrationID) &&
		(r.Harness == "claude" || r.Harness == "codex" || r.Harness == "kimi") && validCanonicalConversationID(r.SessionID) &&
		clientIDPattern.MatchString(r.ClientID) && r.Name != "" && secretHashPattern.MatchString(r.SecretHash) &&
		r.Revision > 0 && !r.CreatedAt.IsZero() && !r.LastSeen.IsZero()
}

// registrationStore keeps one JSON file per registration under a project
// directory. Writes are temp-file+rename so a crash never leaves a half record.
type registrationStore struct {
	database   *sqlitestore.Store
	root       string
	writeFile  func(path string, data []byte) error
	writeBatch func(map[string][]byte) error
	closeRoot  func() error
	closed     bool
	closeErr   error
}

func newRegistrationStore(root string) (*registrationStore, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("registration store root must be an absolute path")
	}
	operationRoot, closeRoot, err := openRegistrationRoot(root)
	if err != nil {
		return nil, errors.New("registration store root is unavailable")
	}
	return &registrationStore{root: operationRoot, closeRoot: closeRoot}, nil
}

// The owning RemoteService serializes close against store operations with mu.
func (st *registrationStore) close() error {
	if !st.closed {
		st.closed = true
		if st.database != nil {
			st.closeErr = st.database.Close()
		}
		st.closeErr = errors.Join(st.closeErr, st.closeRoot())
	}
	return st.closeErr
}

func (st *registrationStore) path(id string) string {
	return filepath.Join(st.root, id+".json")
}

func (st *registrationStore) save(record *registrationRecord) error {
	if st.closed {
		return fs.ErrClosed
	}
	data, err := json.MarshalIndent(record, "", " ")
	if err != nil {
		return err
	}
	if st.writeFile != nil {
		return st.writeFile(st.path(record.RegistrationID), data)
	}
	if st.database != nil {
		return st.database.Put(record.RegistrationID, data)
	}
	return atomicWriteFile(st.path(record.RegistrationID), data)
}

func (st *registrationStore) remove(id string) error {
	if st.closed {
		return fs.ErrClosed
	}
	if st.database != nil {
		return st.database.Delete(id)
	}
	err := pfs.Remove(st.path(id))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (st *registrationStore) quarantine(name string, now time.Time) {
	if st.closed {
		return
	}
	if st.database != nil {
		_ = st.quarantineSQLite(strings.TrimSuffix(name, ".json"), now)
		return
	}
	path := filepath.Join(st.root, name)
	_ = pfs.Rename(path, path+".corrupt-"+now.UTC().Format("20060102T150405.000000000"))
}

// loadAll reads every record. Unreadable or invalid files are moved aside so a
// single corrupt record never blocks the remaining registrations.
func (st *registrationStore) loadAll(now time.Time) ([]registrationRecord, error) {
	if st.closed {
		return nil, fs.ErrClosed
	}
	if st.database != nil {
		return st.loadSQLite(now)
	}
	entries, err := pfs.ReadDir(st.root)
	if err != nil {
		return nil, errors.New("registration store root is unreadable")
	}
	var records []registrationRecord
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := readRegistrationFile(filepath.Join(st.root, name))
		if err != nil {
			st.quarantine(name, now)
			continue
		}
		var record registrationRecord
		if json.Unmarshal(data, &record) != nil || !record.valid() || record.RegistrationID+".json" != name {
			st.quarantine(name, now)
			continue
		}
		records = append(records, record)
	}
	return records, nil
}

func readRegistrationFile(path string) ([]byte, error) {
	file, err := pfs.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func atomicWriteFile(path string, data []byte) (err error) {
	file, err := pfs.CreateTemp(filepath.Dir(path), ".registration-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() {
		if err != nil {
			_ = pfs.Remove(temporary)
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
	return pfs.Rename(temporary, path)
}
