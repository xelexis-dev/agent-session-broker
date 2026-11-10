package broker

import (
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"path/filepath"
	"sort"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

// NewSQLitePersistentRemoteService stores registration bindings in SQLite.
// Valid legacy JSON records are imported once and retained as backups.
func NewSQLitePersistentRemoteService(root string) (*RemoteService, error) {
	if root == "" {
		return nil, errors.New("SQLite registration root is required")
	}
	return newRemoteServiceWithStore(root, true, true)
}

// NewSQLitePersistentRemoteServiceWithBatchCleanup installs policy before startup sweeps.
func NewSQLitePersistentRemoteServiceWithBatchCleanup(root string, cleanup func([]access.DetachedRemovalProof) error) (*RemoteService, error) {
	if root == "" {
		return nil, errors.New("SQLite registration root is required")
	}
	return newRemoteServiceWithCleanup(root, true, true, cleanup)
}

func (st *registrationStore) openSQLite(now time.Time) error {
	database, err := sqlitestore.Open(st.root, "registrations.db")
	if err != nil {
		return err
	}
	initialized, err := database.Initialized()
	if err != nil {
		return errors.Join(err, database.Close())
	}
	if !initialized {
		// database is assigned only after import, so loadAll uses the legacy reader
		// and its existing validation/quarantine rules.
		records, err := st.loadAll(now)
		if err != nil {
			return errors.Join(err, database.Close())
		}
		payloads := make(map[string][]byte, len(records))
		for _, record := range records {
			data, err := json.Marshal(record)
			if err != nil {
				return errors.Join(err, database.Close())
			}
			payloads[record.RegistrationID] = data
		}
		if err := database.Initialize(payloads); err != nil {
			return errors.Join(err, database.Close())
		}
	}
	st.database = database
	return nil
}

func (st *registrationStore) loadSQLite(now time.Time) ([]registrationRecord, error) {
	payloads, err := st.database.List()
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(payloads))
	for key := range payloads {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	records := make([]registrationRecord, 0, len(keys))
	for _, key := range keys {
		var record registrationRecord
		if json.Unmarshal(payloads[key], &record) != nil || !record.valid() || record.RegistrationID != key {
			if err := st.quarantineSQLite(key, now); err != nil {
				return nil, err
			}
			continue
		}
		records = append(records, record)
	}
	return records, nil
}

func (st *registrationStore) quarantineSQLite(id string, now time.Time) error {
	// SQL keys are untrusted disk input. Never turn arbitrary keys into paths.
	if !registrationIDPattern.MatchString(id) {
		return errors.New("invalid SQLite registration key")
	}
	payload, err := st.database.Get(id)
	if err != nil {
		return err
	}
	path := filepath.Join(st.root, id+".json.corrupt-"+now.UTC().Format("20060102T150405.000000000"))
	// Preserve a reviewable copy before removing the active row. On either
	// failure, startup fails closed instead of silently discarding the record.
	if err := atomicWriteFile(path, payload); err != nil {
		return err
	}
	return st.database.Delete(id)
}
