package sqlitestore

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestMisuseD5CorruptionClassificationKeepsBytes(t *testing.T) {
	for _, snapshot := range []bool{false, true} {
		for _, payload := range [][]byte{[]byte("bad"), []byte("synthetic invalid database header"), append([]byte("SQLite format 3\x00"), make([]byte, 128)...)} {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.db")
			if os.WriteFile(path, payload, 0600) != nil {
				t.Fatal("fixture failed")
			}
			open := Open
			if snapshot {
				open = openSnapshot
			}
			st, err := open(dir, "state.db")
			if st != nil {
				st.Close()
			}
			if !errors.Is(err, ErrCorrupt) {
				t.Fatal("invalid database contents not classified as corrupt")
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(payload) {
				t.Fatal("corrupt database was overwritten")
			}
		}
	}
	if errors.Is(classifyDatabaseError(os.ErrPermission), ErrCorrupt) {
		t.Fatal("permission failure classified as corruption")
	}
}

func TestMisuseD6CrashHelper(t *testing.T) {
	dir := os.Getenv("ASB_R2_SQLITE_CHILD_DIR")
	if dir == "" {
		return
	}
	mode := os.Getenv("ASB_R2_SQLITE_CHILD_MODE")
	var st *Store
	var err error
	if mode == "snapshot" {
		st, err = openSnapshot(dir, "state.db")
	} else {
		st, err = Open(dir, "state.db")
	}
	if err != nil {
		os.Exit(20)
	}
	if mode == "native" {
		tx, err := st.db.Begin()
		if err != nil {
			os.Exit(21)
		}
		for _, key := range []string{"a", "b"} {
			if _, err = tx.Exec("UPDATE records SET payload=? WHERE key=?", []byte("uncommitted"), key); err != nil {
				os.Exit(22)
			}
		}
		if os.WriteFile(filepath.Join(dir, "ready"), []byte("ready"), 0600) != nil {
			os.Exit(23)
		}
		time.Sleep(time.Hour)
	} else {
		if os.WriteFile(filepath.Join(dir, "ready"), []byte("ready"), 0600) != nil {
			os.Exit(24)
		}
		for n := 0; ; n++ {
			value := []byte(fmt.Sprint(n))
			if st.Batch(map[string][]byte{"a": value, "b": value}, nil) != nil {
				os.Exit(25)
			}
		}
	}
	os.Exit(26)
}

func TestMisuseD6KilledWriterLeavesWholeTransaction(t *testing.T) {
	for _, mode := range []string{"native", "snapshot"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			open := Open
			if mode == "snapshot" {
				open = openSnapshot
			}
			st, err := open(dir, "state.db")
			if err != nil {
				t.Fatal(err)
			}
			if st.Batch(map[string][]byte{"a": []byte("before"), "b": []byte("before")}, nil) != nil {
				t.Fatal("fixture commit failed")
			}
			st.Close()
			cmd := exec.Command(os.Args[0], "-test.run=^TestMisuseD6CrashHelper$")
			cmd.Env = append(os.Environ(), "ASB_R2_SQLITE_CHILD_DIR="+dir, "ASB_R2_SQLITE_CHILD_MODE="+mode)
			if err = cmd.Start(); err != nil {
				t.Fatal("owned child start failed")
			}
			defer func() { _ = cmd.Process.Kill() }()
			ready := false
			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
					ready = true
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if !ready {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal("owned writer did not reach transaction boundary")
			}
			if mode == "snapshot" {
				time.Sleep(20 * time.Millisecond)
			}
			if cmd.Process.Kill() != nil {
				t.Fatal("owned writer kill failed")
			}
			_ = cmd.Wait()
			st, err = open(dir, "state.db")
			if err != nil {
				t.Fatal("killed writer made database unreadable")
			}
			defer st.Close()
			a, ea := st.Get("a")
			b, eb := st.Get("b")
			if ea != nil || eb != nil || string(a) != string(b) {
				t.Fatal("partial transaction survived kill")
			}
			if mode == "native" && string(a) != "before" {
				t.Fatal("uncommitted transaction survived kill")
			}
		})
	}
}

func TestMisuseD6SQLiteFullRollsBackAllRows(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, "state.db")
	if err != nil {
		t.Fatal(err)
	}
	if st.Batch(map[string][]byte{"a": []byte("before"), "b": []byte("before")}, nil) != nil {
		t.Fatal("fixture failed")
	}
	var pages int
	if st.db.QueryRow("PRAGMA page_count").Scan(&pages) != nil {
		t.Fatal("page count unavailable")
	}
	if _, err = st.db.Exec(fmt.Sprintf("PRAGMA max_page_count=%d", pages)); err != nil {
		t.Fatal("full database simulation unavailable")
	}
	if err = st.Batch(map[string][]byte{"a": make([]byte, 1<<20), "b": []byte("after")}, nil); err == nil {
		t.Fatal("database-full transaction succeeded")
	}
	var coded interface{ Code() int }
	if !errors.As(err, &coded) || coded.Code()&255 != 13 {
		t.Fatal("write failed for a reason other than SQLITE_FULL")
	}

	a, ea := st.Get("a")
	b, eb := st.Get("b")
	if ea != nil || eb != nil || string(a) != "before" || string(b) != "before" {
		t.Fatal("database-full transaction partially published")
	}
	st.Close()
	reopened, err := Open(dir, "state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	a, ea = reopened.Get("a")
	b, eb = reopened.Get("b")
	if ea != nil || eb != nil || string(a) != "before" || string(b) != "before" {
		t.Fatal("database-full transaction survived restart partially")
	}
}
