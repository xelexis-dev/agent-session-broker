package bugreport

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

func TestUnavailableStoreReturnsOneFixedError(t *testing.T) {
	s := Unavailable()
	defer s.Close()
	if !s.IsUnavailable() {
		t.Fatal("unavailable marker missing")
	}
	if _, err := s.Create(fixtureInput(), reporter(1), "test"); err != ErrUnavailable {
		t.Fatal("create error differs")
	}
	if _, _, err := s.List(Filter{}, 1); err != ErrUnavailable {
		t.Fatal("list error differs")
	}
	if _, err := s.Get("bug-fixture"); err != ErrUnavailable {
		t.Fatal("get error differs")
	}
	if err := s.SetStatus("bug-fixture", "closed", "admin"); err != ErrUnavailable {
		t.Fatal("status error differs")
	}
	if err := s.DeleteProject("project", "AAAAAAAAAAAAAAAAAAAAAAAAAA"); err != ErrUnavailable {
		t.Fatal("delete error differs")
	}
	if ErrUnavailable.Error() != "bug report storage is unavailable" {
		t.Fatal("fixed message changed")
	}
}

func TestOpenDistinguishesCorruptionFromOrdinaryFailure(t *testing.T) {
	for _, kind := range []string{"corrupt", "directory", "schema"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "bugreports")
			if os.MkdirAll(dir, 0700) != nil {
				t.Fatal("fixture failed")
			}
			path := filepath.Join(dir, "bugreports.db")
			switch kind {
			case "corrupt":
				if os.WriteFile(path, []byte("synthetic bad SQLite header"), 0600) != nil {
					t.Fatal("fixture failed")
				}
			case "directory":
				if os.Mkdir(path, 0700) != nil {
					t.Fatal("fixture failed")
				}
			case "schema":
				db, err := sqlitestore.Open(dir, "bugreports.db")
				if err != nil {
					t.Fatal(err)
				}
				db.Close()
				// Patch only the synthetic SQLite header user_version word, big endian.
				f, err := os.OpenFile(path, os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.WriteAt([]byte{0, 0, 3, 231}, 60); err != nil {
					t.Fatal(err)
				}
				f.Close()
			}
			st, err := Open(root)
			if st != nil {
				st.Close()
			}
			if err == nil || errors.Is(err, sqlitestore.ErrCorrupt) != (kind == "corrupt") {
				t.Fatal("ordinary failure/corruption classification changed")
			}
		})
	}
}

func TestProjectFilterHidesOrphansBeforePaginationAndStatus(t *testing.T) {
	store, _ := fixtureStore(t)
	live := map[string]bool{"project": true}
	orphan := reporter(2)
	orphan.Project = "deleted"
	hidden, err := store.Create(fixtureInput(), orphan, "test")
	if err != nil {
		t.Fatal(err)
	}
	visible, err := store.Create(fixtureInput(), reporter(1), "test")
	if err != nil {
		t.Fatal(err)
	}
	store.SetProjectFilter(func(project string) bool { return live[project] })
	rows, total, err := store.List(Filter{}, 1)
	if err != nil || total != 1 || len(rows) != 1 || rows[0].ID != visible.ID {
		t.Fatal("orphan included in pagination/count")
	}
	if _, err = store.Get(hidden.ID); err != ErrNotFound {
		t.Fatal("orphan detail visible")
	}
	if err = store.SetStatus(hidden.ID, "closed", "admin"); err != ErrNotFound {
		t.Fatal("orphan status mutated")
	}
}
