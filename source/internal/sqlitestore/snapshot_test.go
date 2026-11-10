package sqlitestore

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSnapshotFailureKeepsLiveAndRestartState(t *testing.T) {
	dir := t.TempDir()
	st, err := openSnapshot(dir, "state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Initialize(map[string][]byte{"key": []byte("before")}); err != nil {
		t.Fatal(err)
	}
	// Rename cannot overwrite a directory: this fails at publication after SQL
	// mutation and serialization have completed in the isolated staging DB.
	actual := st.snapshotPath
	if err := os.Mkdir(filepath.Join(dir, "blocked"), 0700); err != nil {
		t.Fatal(err)
	}
	st.snapshotPath = filepath.Join(filepath.Dir(actual), "blocked")
	if err := st.Put("key", []byte("after")); err == nil {
		t.Fatal("failed snapshot reported success")
	}
	st.snapshotPath = actual
	data, err := st.Get("key")
	if err != nil || string(data) != "before" {
		t.Fatalf("live state changed: %q %v", data, err)
	}
	st.Close()
	reopened, err := openSnapshot(dir, "state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	data, err = reopened.Get("key")
	if err != nil || string(data) != "before" {
		t.Fatalf("durable state changed: %q %v", data, err)
	}
}

func TestSnapshotDirectoryReplacementKeepsPinnedDestination(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory handles prevent rename; Unix capability behavior")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "original")
	st, err := openSnapshot(dir, "state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	parked := filepath.Join(parent, "parked")
	if err := os.Rename(dir, parked); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	if err := st.Put("key", []byte("pinned")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside write: %v %v", entries, err)
	}
	st.Close()
	reopened, err := openSnapshot(parked, "state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	value, err := reopened.Get("key")
	if err != nil || string(value) != "pinned" {
		t.Fatalf("pinned data lost: %q %v", value, err)
	}
}

func TestSnapshotRejectsSymlinkChildOfCapability(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux procfs capability")
	}
	parent, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	capability := fmt.Sprintf("/proc/self/fd/%d", parent.Fd())
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(capability, "registrations")); err != nil {
		t.Fatal(err)
	}
	if st, err := Open(filepath.Join(capability, "registrations"), "state.db"); err == nil {
		st.Close()
		t.Fatal("symlink child accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside write: %v %v", entries, err)
	}
}
