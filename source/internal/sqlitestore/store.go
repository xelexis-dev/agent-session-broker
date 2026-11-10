// Package sqlitestore provides the server's transactional durable key store.
package sqlitestore

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
	_ "modernc.org/sqlite"
)

const schemaVersion = 1

// ErrCorrupt identifies verified invalid database contents, not lock, permission
// or schema-version failures. Runtimes may isolate the affected project.
var ErrCorrupt = errors.New("SQLite database is corrupt")

func classifyDatabaseError(err error) error {
	var coded interface{ Code() int }
	// SQLite primary result codes: CORRUPT=11, NOTADB=26.
	if errors.As(err, &coded) && (coded.Code()&255 == 11 || coded.Code()&255 == 26) {
		return ErrCorrupt
	}
	return err
}

type Store struct {
	mu            sync.Mutex
	db            *sql.DB
	directory     *os.File
	directoryPath string
	filePath      string
	fileInfo      os.FileInfo
	closed        bool
	closeErr      error
	snapshotPath  string
}

// Open owns its directory handle until Close. Database and journal files stay
// in that directory; a changed directory/file identity fails closed.
func Open(directory, filename string) (_ *Store, err error) {
	if !filepath.IsAbs(directory) || filename == "" || filename == "." || filename == ".." || strings.ContainsAny(filename, `/\?`+"\x00") {
		return nil, errors.New("SQLite requires an absolute directory and a plain filename")
	}
	if strings.HasPrefix(directory, "/dev/fd/") || strings.HasPrefix(directory, "/proc/self/fd/") {
		return openSnapshot(directory, filename)
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	info, err := pfs.Lstat(directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("SQLite directory is not a directory")
	}
	pin, err := pfs.Open(directory)
	if err != nil {
		return nil, err
	}
	st := &Store{directory: pin}
	defer func() {
		if err != nil {
			_ = st.Close()
		}
	}()
	resolved, err := pfs.EvalSymlinks(directory)
	if err != nil {
		return nil, err
	}
	st.directoryPath = resolved
	st.filePath = filepath.Join(resolved, filename)
	pinInfo, err := pin.Stat()
	if err != nil {
		return nil, err
	}
	resolvedInfo, err := os.Lstat(resolved)
	if err != nil || !resolvedInfo.IsDir() || !os.SameFile(pinInfo, resolvedInfo) {
		return nil, errors.New("SQLite directory changed while opening")
	}
	if err := pin.Chmod(0700); err != nil {
		return nil, err
	}
	existed := true
	file, err := os.OpenFile(st.filePath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, fs.ErrExist) {
		info, err := os.Lstat(st.filePath)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("SQLite database is not a regular file")
		}
		file, err = os.OpenFile(st.filePath, os.O_RDWR, 0)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else {
		existed = false
	}
	st.fileInfo, err = file.Stat()
	if err == nil {
		err = file.Chmod(0600)
	}
	if err == nil && existed {
		var header [16]byte
		_, err = io.ReadFull(file, header[:])
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			err = ErrCorrupt
		}
		if err == nil && string(header[:]) != "SQLite format 3\x00" {
			err = ErrCorrupt
		}
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return nil, err
	}
	if err = st.check(); err != nil {
		return nil, err
	}
	path := filepath.ToSlash(st.filePath)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	uri := url.URL{Scheme: "file", Path: path}
	query := url.Values{"mode": {"rw"}, "_pragma": {"busy_timeout(5000)", "synchronous(FULL)"}, "_txlock": {"immediate"}}
	uri.RawQuery = query.Encode()
	st.db, err = sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	st.db.SetMaxOpenConns(1)
	st.db.SetMaxIdleConns(1)
	var check string
	if err = st.db.QueryRow("PRAGMA quick_check").Scan(&check); err != nil {
		return nil, classifyDatabaseError(err)
	}
	if check != "ok" {
		return nil, ErrCorrupt
	}
	var version int
	if err = st.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if existed && version != schemaVersion {
		return nil, fmt.Errorf("unsupported SQLite schema version %d", version)
	}
	if !existed {
		tx, e := st.db.Begin()
		if e != nil {
			return nil, e
		}
		defer tx.Rollback()
		for _, statement := range []string{
			"CREATE TABLE records (key TEXT PRIMARY KEY NOT NULL, payload BLOB NOT NULL)",
			"CREATE TABLE metadata (key TEXT PRIMARY KEY NOT NULL, value INTEGER NOT NULL)",
			"PRAGMA user_version = 1",
		} {
			if _, err = tx.Exec(statement); err != nil {
				return nil, err
			}
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
	}
	// Validate required columns as well as the version before serving any data.
	for _, statement := range []string{"SELECT key,payload FROM records LIMIT 0", "SELECT key,value FROM metadata LIMIT 0"} {
		rows, e := st.db.Query(statement)
		if e != nil {
			return nil, e
		}
		if err = rows.Close(); err != nil {
			return nil, err
		}
	}
	var journal string
	if err = st.db.QueryRow("PRAGMA journal_mode = DELETE").Scan(&journal); err != nil {
		return nil, err
	}
	if journal != "delete" {
		return nil, errors.New("SQLite rollback journal unavailable")
	}
	return st, nil
}

func (s *Store) check() error {
	if s.closed {
		return fs.ErrClosed
	}
	if s.snapshotPath != "" {
		_, err := s.directory.Stat()
		return err
	}
	current, err := os.Lstat(s.directoryPath)
	if err != nil {
		return err
	}
	pinned, err := s.directory.Stat()
	if err != nil {
		return err
	}
	if !current.IsDir() || !os.SameFile(current, pinned) {
		return errors.New("SQLite directory identity changed")
	}
	file, err := os.Lstat(s.filePath)
	if err != nil {
		return err
	}
	if !file.Mode().IsRegular() || !os.SameFile(file, s.fileInfo) {
		return errors.New("SQLite database identity changed")
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		info, err := os.Lstat(s.filePath + suffix)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err == nil && !info.Mode().IsRegular() {
			return errors.New("unsafe SQLite sidecar")
		}
	}
	return nil
}

func (s *Store) Get(key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return nil, err
	}
	var data []byte
	err := s.db.QueryRow("SELECT payload FROM records WHERE key = ?", key).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		err = fs.ErrNotExist
	}
	return data, err
}

func (s *Store) Put(key string, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return err
	}
	return s.mutate(func(db *sql.DB) error {
		_, err := db.Exec("INSERT INTO records(key,payload) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET payload=excluded.payload", key, payload)
		return err
	})
}

func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return err
	}
	return s.mutate(func(db *sql.DB) error {
		_, err := db.Exec("DELETE FROM records WHERE key = ?", key)
		return err
	})
}

func (s *Store) List() (map[string][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return nil, err
	}
	rows, err := s.db.Query("SELECT key,payload FROM records ORDER BY key")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := map[string][]byte{}
	for rows.Next() {
		var key string
		var payload []byte
		if err := rows.Scan(&key, &payload); err != nil {
			return nil, err
		}
		records[key] = payload
	}
	return records, rows.Err()
}

func (s *Store) Initialized() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return false, err
	}
	return initialized(s.db)
}

type rowQuerier interface{ QueryRow(string, ...any) *sql.Row }

func initialized(db rowQuerier) (bool, error) {
	var value int
	err := db.QueryRow("SELECT value FROM metadata WHERE key = 'initialized'").Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if value != 1 {
		return false, errors.New("invalid SQLite migration marker")
	}
	return true, nil
}

// Initialize imports legacy records and the completion marker in one commit.
// Once committed, even an empty records table never imports legacy data again.
func (s *Store) Initialize(records map[string][]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return err
	}
	return s.mutate(func(db *sql.DB) error { return initialize(db, records) })
}

func initialize(db *sql.DB, records map[string][]byte) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	complete, err := initialized(tx)
	if err != nil {
		return err
	}
	if complete {
		return nil
	}
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := tx.Exec("INSERT INTO records(key,payload) VALUES(?,?)", key, records[key]); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("INSERT INTO metadata(key,value) VALUES('initialized',1)"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		if s.db != nil {
			s.closeErr = s.db.Close()
		}
		if s.directory != nil {
			s.closeErr = errors.Join(s.closeErr, s.directory.Close())
		}
	}
	return s.closeErr
}

// Batch publishes all records in one transaction, including the pinned-root
// snapshot path. A failed statement or durable snapshot leaves every key old.
func (s *Store) Batch(puts map[string][]byte, deletes []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return err
	}
	return s.mutate(func(db *sql.DB) error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, key := range deletes {
			if _, err = tx.Exec("DELETE FROM records WHERE key=?", key); err != nil {
				return err
			}
		}
		for key, payload := range puts {
			if _, err = tx.Exec("INSERT INTO records(key,payload) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET payload=excluded.payload", key, payload); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}
