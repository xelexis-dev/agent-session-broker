package sqlitestore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestSQLiteReopenAndMigrationDoesNotReviveDeletedRecords(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, "state.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Initialize(map[string][]byte{"old": []byte("legacy")}); err != nil {
		t.Fatal(err)
	}
	if err := st.Put("new", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := st.Put("new", []byte("updated")); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete("old"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	header, err := os.ReadFile(filepath.Join(dir, "state.db"))
	if err != nil || len(header) < 16 || string(header[:16]) != "SQLite format 3\x00" {
		t.Fatalf("not a SQLite database: %v", err)
	}
	st, err = Open(dir, "state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Initialize(map[string][]byte{"old": []byte("revived")}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get("old"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deleted legacy revived: %v", err)
	}
	data, err := st.Get("new")
	if err != nil || string(data) != "updated" {
		t.Fatalf("reopen: %q %v", data, err)
	}
	records, err := st.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("list: %v %v", records, err)
	}
}

func TestSQLiteFailedImportRollsBackRowsAndMarker(t *testing.T) {
	st, err := Open(t.TempDir(), "state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// The NULL payload violates the real database constraint after an insert.
	if err := st.Initialize(map[string][]byte{"a": []byte("valid"), "z": nil}); err == nil {
		t.Fatal("invalid import succeeded")
	}
	initialized, err := st.Initialized()
	if err != nil || initialized {
		t.Fatalf("failed import marked complete: %v %v", initialized, err)
	}
	rows, err := st.List()
	if err != nil || len(rows) != 0 {
		t.Fatalf("partial import: %v %v", rows, err)
	}
	if err := st.Initialize(map[string][]byte{"a": []byte("retry")}); err != nil {
		t.Fatal(err)
	}
	if err := st.Put("a", nil); err == nil {
		t.Fatal("invalid update succeeded")
	}
	data, err := st.Get("a")
	if err != nil || string(data) != "retry" {
		t.Fatalf("failed update replaced state: %q %v", data, err)
	}
}

func TestSQLiteRejectsCorruptAndUnknownSchemaWithoutReset(t *testing.T) {
	t.Run("corrupt", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.db")
		original := []byte("this is not a database")
		if err := os.WriteFile(path, original, 0600); err != nil {
			t.Fatal(err)
		}
		if st, err := Open(dir, "state.db"); err == nil {
			st.Close()
			t.Fatal("corrupt database accepted")
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(original) {
			t.Fatalf("corrupt database overwritten: %q %v", after, err)
		}
	})
	t.Run("future schema", func(t *testing.T) {
		dir := t.TempDir()
		st, err := Open(dir, "state.db")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.Exec("PRAGMA user_version = 999"); err != nil {
			t.Fatal(err)
		}
		st.Close()
		if st, err := Open(dir, "state.db"); err == nil {
			st.Close()
			t.Fatal("unknown schema accepted")
		}
	})
}

func TestSQLiteRejectsClosedStoreAndUnsafePaths(t *testing.T) {
	st, err := Open(t.TempDir(), "state.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Put("key", []byte("value")); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("closed write: %v", err)
	}
	for _, name := range []string{"../escape.db", "", "sub/state.db"} {
		if st, err := Open(t.TempDir(), name); err == nil {
			st.Close()
			t.Fatalf("unsafe name accepted: %q", name)
		}
	}
}

func TestSQLiteInvalidMigrationMarkerFailsClosed(t *testing.T) {
	st, err := Open(t.TempDir(), "state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Initialize(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec("UPDATE metadata SET value = 999 WHERE key = 'initialized'"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Initialized(); err == nil {
		t.Fatal("invalid marker accepted")
	}
	if err := st.Initialize(map[string][]byte{"legacy": []byte("record")}); err == nil {
		t.Fatal("invalid marker allowed migration")
	}
}

func TestBatchRollsBackEveryRecordOnConstraintFailure(t *testing.T) {
	st, err := Open(t.TempDir(), "batch.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = st.Put("state", []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err = st.Batch(map[string][]byte{"state": []byte("new"), "audit/fail": nil}, []string{"state"}); err == nil {
		t.Fatal("invalid payload accepted")
	}
	raw, err := st.Get("state")
	if err != nil || string(raw) != "old" {
		t.Fatal("failed batch changed state")
	}
	if _, err = st.Get("audit/fail"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("failed batch persisted audit")
	}
	if err = st.Batch(map[string][]byte{"state": []byte("new"), "audit/ok": []byte("event")}, nil); err != nil {
		t.Fatal(err)
	}
	raw, _ = st.Get("state")
	if string(raw) != "new" {
		t.Fatal("batch state missing")
	}
	raw, _ = st.Get("audit/ok")
	if string(raw) != "event" {
		t.Fatal("batch audit missing")
	}
}
