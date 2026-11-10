package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
)

type serializer interface {
	Serialize() ([]byte, error)
	Deserialize([]byte) error
}

// Capability paths need descriptor-relative I/O, which SQLite's native VFS
// does not provide on Darwin. A staged SQLite snapshot preserves those paths
// without ever publishing a mutation before its durable file replacement.
func openSnapshot(directory, filename string) (_ *Store, err error) {
	if strings.HasPrefix(directory, "/dev/fd/") {
		// A bare descriptor already names an open directory. Mkdir on the
		// capability itself is deliberately rejected by pinnedpath.
		if filepath.Dir(directory) != "/dev/fd" {
			if err := pfs.Mkdir(directory, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
				return nil, err
			}
		}
	} else if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	info, err := pfs.Lstat(directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() && !(filepath.Dir(directory) == "/proc/self/fd" && info.Mode()&os.ModeSymlink != 0) {
		return nil, errors.New("SQLite snapshot directory is not a directory")
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
	if err := pin.Chmod(0700); err != nil {
		return nil, err
	}
	switch runtime.GOOS {
	case "darwin":
		directory = fmt.Sprintf("/dev/fd/%d", pin.Fd())
	case "linux":
		directory = fmt.Sprintf("/proc/self/fd/%d", pin.Fd())
	}
	st.snapshotPath = filepath.Join(directory, filename)
	var data []byte
	info, err = pfs.Lstat(st.snapshotPath)
	if err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("SQLite snapshot is not a regular file")
		}
		file, e := pfs.Open(st.snapshotPath)
		if e != nil {
			return nil, e
		}
		if e := file.Chmod(0600); e != nil {
			file.Close()
			return nil, e
		}
		data, err = io.ReadAll(file)
		err = errors.Join(err, file.Close())
		if err != nil {
			return nil, err
		}
		if len(data) < 16 || string(data[:16]) != "SQLite format 3\x00" {
			return nil, ErrCorrupt
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	st.db, err = memoryDatabase(data)
	if err != nil {
		return nil, classifyDatabaseError(err)
	}
	if data == nil {
		if _, err = st.db.Exec("CREATE TABLE records (key TEXT PRIMARY KEY NOT NULL, payload BLOB NOT NULL); CREATE TABLE metadata (key TEXT PRIMARY KEY NOT NULL, value INTEGER NOT NULL); PRAGMA user_version = 1"); err != nil {
			return nil, err
		}
		data, err = serialize(st.db)
		if err != nil {
			return nil, err
		}
		if err = writeSnapshot(st.snapshotPath, data); err != nil {
			return nil, err
		}
	}
	var result string
	if err = st.db.QueryRow("PRAGMA quick_check").Scan(&result); err != nil {
		return nil, classifyDatabaseError(err)
	}
	if result != "ok" {
		return nil, ErrCorrupt
	}
	var version int
	if err = st.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if version != schemaVersion {
		return nil, fmt.Errorf("unsupported SQLite schema version %d", version)
	}
	for _, statement := range []string{"SELECT key,payload FROM records LIMIT 0", "SELECT key,value FROM metadata LIMIT 0"} {
		rows, e := st.db.Query(statement)
		if e != nil {
			return nil, e
		}
		if err = rows.Close(); err != nil {
			return nil, err
		}
	}
	return st, nil
}

func memoryDatabase(data []byte) (*sql.DB, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if data != nil {
		conn, err := db.Conn(context.Background())
		if err != nil {
			db.Close()
			return nil, err
		}
		err = conn.Raw(func(driver any) error { return driver.(serializer).Deserialize(data) })
		err = errors.Join(err, conn.Close())
		if err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

func serialize(db *sql.DB) ([]byte, error) {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var data []byte
	err = conn.Raw(func(driver any) error { var err error; data, err = driver.(serializer).Serialize(); return err })
	return data, err
}

func (s *Store) mutate(change func(*sql.DB) error) error {
	if s.snapshotPath == "" {
		return change(s.db)
	}
	before, err := serialize(s.db)
	if err != nil {
		return err
	}
	staging, err := memoryDatabase(before)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = staging.Close()
		}
	}()
	if err := change(staging); err != nil {
		return err
	}
	after, err := serialize(staging)
	if err != nil {
		return err
	}
	if err := writeSnapshot(s.snapshotPath, after); err != nil {
		return err
	}
	previous := s.db
	s.db = staging
	keep = true
	_ = previous.Close()
	return nil
}

func writeSnapshot(path string, data []byte) (err error) {
	file, err := pfs.CreateTemp(filepath.Dir(path), ".sqlite-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() {
		if err != nil {
			_ = file.Close()
			_ = pfs.Remove(temporary)
		}
	}()
	if _, err = file.Write(data); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return pfs.Rename(temporary, path)
}
