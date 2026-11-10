package clientstate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func pruneFile(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("unread synthetic content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}
func pruneExists(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Lstat(path)
	if (err == nil) != want || (err != nil && !os.IsNotExist(err)) {
		t.Fatalf("exists=%t, want=%t", err == nil, want)
	}
}
func TestPruneRetentionLocksStampAndNames(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-31 * 24 * time.Hour)
	key := func(id string) string { return SessionKey("synthetic", "codex", id, "pc-synthetic") }
	session := func(id string) string { return filepath.Join(dir, "sessions", key(id)) }
	for _, id := range []string{"old", "locked", "recent"} {
		at := old
		if id == "recent" {
			at = now
		}
		pruneFile(t, filepath.Join(session(id), "conn.json"), at)
	}
	unlock, err := s.LockSession(key("locked"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	missing := session("missing")
	if err := os.MkdirAll(missing, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(missing, old, old); err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(dir, "sessions", "INVALID")
	pruneFile(t, filepath.Join(invalid, "conn.json"), old)
	for _, id := range []string{"old", "recent", "locked"} {
		at := old
		if id == "recent" {
			at = now
		}
		pruneFile(t, filepath.Join(dir, "registrations", key(id)+".json"), at)
		at = now.Add(-15 * 24 * time.Hour)
		if id == "recent" {
			at = now
		}
		pruneFile(t, filepath.Join(dir, "diagnostics", key(id)+".json"), at)
	}
	releaseReg, err := s.Lock(key("locked"))
	if err != nil {
		t.Fatal(err)
	}
	defer releaseReg()
	releaseDiag, err := lockFile(filepath.Join(dir, "diagnostics", key("locked")+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDiag()
	result, err := s.Prune(now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Sessions != 2 || result.Registrations != 1 || result.Diagnostics != 1 {
		t.Fatalf("counts=%+v", result)
	}
	for _, id := range []string{"old", "missing"} {
		pruneExists(t, session(id), false)
	}
	for _, id := range []string{"locked", "recent"} {
		pruneExists(t, session(id), true)
	}
	pruneExists(t, invalid, true)
	for _, kind := range []string{"registrations", "diagnostics"} {
		pruneExists(t, filepath.Join(dir, kind, key("old")+".json"), false)
		pruneExists(t, filepath.Join(dir, kind, key("old")+".lock"), false)
		for _, id := range []string{"locked", "recent"} {
			pruneExists(t, filepath.Join(dir, kind, key(id)+".json"), true)
		}
	}
	pruneFile(t, filepath.Join(session("second"), "conn.json"), old)
	result, err = s.Prune(now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if result.Skipped != 1 || result.Sessions != 0 {
		t.Fatalf("stamp guard=%+v", result)
	}
	pruneExists(t, session("second"), true)
	_, err = s.Prune(now.Add(25 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	pruneExists(t, session("second"), false)
}

func TestPruneSkipsRunLockAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-31 * 24 * time.Hour)
	key := SessionKey("synthetic", "codex", "old", "pc-synthetic")
	target := filepath.Join(dir, "sessions", key)
	pruneFile(t, filepath.Join(target, "conn.json"), old)
	release, err := lockFile(filepath.Join(dir, "prune.lock"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Prune(now)
	release()
	if err != nil || result.Skipped != 1 {
		t.Fatal("run lock not skipped")
	}
	pruneExists(t, target, true)
	outside := t.TempDir()
	pruneFile(t, filepath.Join(outside, "conn.json"), old)
	for i, kind := range []string{"sessions", "registrations", "diagnostics"} {
		name := SessionKey("synthetic", kind, "link", "pc-synthetic")
		parent := filepath.Join(dir, kind)
		if err := os.MkdirAll(parent, 0700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(parent, name)
		dest := outside
		if i > 0 {
			link += ".json"
			dest = filepath.Join(outside, "conn.json")
		}
		if err := os.Symlink(dest, link); err != nil {
			t.Skip("symlink creation unavailable")
		}
		defer pruneExists(t, link, true)
	}
	// A valid session with a symlinked state or lock must also be left alone.
	for _, file := range []string{"conn.json", "conn.lock"} {
		name := SessionKey("synthetic", file, "link", "pc-synthetic")
		parent := filepath.Join(dir, "sessions", name)
		if err := os.MkdirAll(parent, 0700); err != nil {
			t.Fatal(err)
		}
		if file == "conn.lock" {
			pruneFile(t, filepath.Join(parent, "conn.json"), old)
		}
		if err := os.Symlink(filepath.Join(outside, "conn.json"), filepath.Join(parent, file)); err != nil {
			t.Fatal(err)
		}
		defer pruneExists(t, parent, true)
	}
	if _, err := s.Prune(now); err != nil {
		t.Fatal(err)
	}
	pruneExists(t, filepath.Join(outside, "conn.json"), true)
}

func TestLoadsRefreshStateModificationTime(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := SessionKey("synthetic", "codex", "old", "pc-synthetic")
	registration := Registration{Secret: "synthetic-proof", RegistrationID: "synthetic-registration"}
	if err := s.SaveSession(key, SessionState{Registration: registration}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRegistration(key, registration); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-31 * 24 * time.Hour)
	paths := []string{filepath.Join(s.sessionDir(key), "conn.json"), s.registrationPath(key)}
	for _, path := range paths {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	before := time.Now().Add(-time.Second)
	if _, found, err := s.LoadSession(key); err != nil || !found {
		t.Fatal("session load failed")
	}
	if _, found, err := s.LoadRegistration(key); err != nil || !found {
		t.Fatal("registration load failed")
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || info.ModTime().Before(before) {
			t.Fatal("load did not refresh state")
		}
	}
	if _, err := s.Prune(time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		pruneExists(t, path, true)
	}
}

func TestPrunePreservesCodexResumeClaims(t *testing.T) {
	root := t.TempDir()
	s, e := Open(root)
	if e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(root, "codex-resume-claims", "42.json")
	pruneFile(t, p, time.Now().Add(-90*24*time.Hour))
	if _, e = s.Prune(time.Now()); e != nil {
		t.Fatal(e)
	}
	pruneExists(t, p, true)
}
