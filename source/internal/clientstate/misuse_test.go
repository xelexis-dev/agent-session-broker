package clientstate

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMisuseStateCorruptionDeletionAndWriteFailure(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal("open failed")
	}
	key := SessionKey("scope", "claude", "synthetic-session", "synthetic-client")
	valid := SessionState{Registration: Registration{Secret: NewSecret(), RegistrationID: "AAAAAAAAAAAAAAAAAAAAAAAAAA", Revision: 1}}
	if err := s.SaveSession(key, valid); err != nil {
		t.Fatal("save failed")
	}
	path := filepath.Join(s.sessionDir(key), "conn.json")
	for _, data := range [][]byte{nil, []byte("{"), {0xff, 0, 0x80}, []byte("null")} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal("inject failed")
		}
		if _, found, err := s.LoadSession(key); found || err == nil || strings.Contains(err.Error(), s.Dir()) {
			t.Fatal("corrupt state did not fail safely")
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal("delete failed")
	}
	if _, found, err := s.LoadSession(key); found || err != nil {
		t.Fatal("missing state not represented as missing")
	}
	if err := s.SaveSession(key, valid); err != nil {
		t.Fatal("deleted state did not recreate")
	}
	// An unreplaceable destination exercises the atomic write failure on all
	// platforms, including tests run as root where chmod alone cannot deny writes.
	if err := os.Remove(path); err != nil {
		t.Fatal("remove fixture failed")
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal("collision fixture failed")
	}
	if err := s.SaveSession(key, valid); err == nil || strings.Contains(err.Error(), s.Dir()) {
		t.Fatal("unwritable destination did not fail safely")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal("clear fixture failed")
	}
	if err := s.SaveSession(key, valid); err != nil {
		t.Fatal("state did not recover after write obstruction")
	}
}

func TestMisuseReadOnlyStateDoesNotReplaceCommittedData(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal("open failed")
	}
	key := SessionKey("scope", "claude", "synthetic-session", "synthetic-client")
	valid := SessionState{Registration: Registration{Secret: NewSecret(), RegistrationID: "AAAAAAAAAAAAAAAAAAAAAAAAAA", Revision: 1}}
	if err := s.SaveSession(key, valid); err != nil {
		t.Fatal("setup failed")
	}
	dir := s.sessionDir(key)
	t.Cleanup(readOnlyStateDirectory(t, dir))
	valid.Registration.Revision = 2
	if err := s.SaveSession(key, valid); err == nil {
		t.Fatal("read-only write succeeded")
	}
	got, found, err := s.LoadSession(key)
	if err != nil || !found || got.Registration.Revision != 1 {
		t.Fatal("failed write changed committed state")
	}
}

func TestMisuseLockChild(t *testing.T) {
	dir := os.Getenv("ASB_TEST_LOCK_CHILD")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal("child open failed")
	}
	release, err := s.LockSession(SessionKey("scope", "claude", "synthetic", "same-client"))
	if err != nil {
		t.Fatal("child lock failed")
	}
	defer release()
	if _, err := os.Stdout.WriteString("locked\n"); err != nil {
		t.Fatal("child handshake failed")
	}
	var b [1]byte
	os.Stdin.Read(b[:])
}

func TestMisuseB6AbruptProcessExitReleasesLock(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal("test executable unavailable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	dir := t.TempDir()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestMisuseLockChild$")
	cmd.Env = append(os.Environ(), "ASB_TEST_LOCK_CHILD="+dir)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("child input failed")
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("child output failed")
	}
	if err := cmd.Start(); err != nil {
		t.Fatal("child start failed")
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatal("child did not acquire lock")
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal("parent open failed")
	}
	key := SessionKey("scope", "claude", "synthetic", "same-client")
	if release, err := s.LockSession(key); !errors.Is(err, ErrLocked) {
		if release != nil {
			release()
		}
		t.Fatal("child lock was not exclusive")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal("child kill failed")
	}
	cmd.Wait()
	release, err := s.LockSession(key)
	if err != nil {
		t.Fatal("abrupt exit left a stale lock")
	}
	release()
}
