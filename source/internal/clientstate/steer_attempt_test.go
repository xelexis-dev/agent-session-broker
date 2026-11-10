package clientstate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const attemptThread = "11111111-1111-4111-8111-111111111111"

func TestSteerAttemptCASAndEmpty(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("daemon steer recovery is not installed on Windows; protected POSIX fixture")
	}
	s, _ := Open(t.TempDir())
	before, err := s.LoadSteerAttempt("broker", attemptThread)
	if err != nil {
		t.Fatal(err)
	}
	next := before
	next.Registration = "R"
	next.Epoch = "E"
	next.Pending = &SteerAttempt{ClientID: "xb:R:E:2-4", Sequences: []uint64{2, 4}, Turns: []string{"T"}}
	if err = s.SaveSteerAttempt("broker", attemptThread, before, next); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveSteerAttempt("broker", attemptThread, before, next); !errors.Is(err, ErrSteerChanged) {
		t.Fatal("stale writer replaced record")
	}
	got, err := s.LoadSteerAttempt("broker", attemptThread)
	if err != nil || got.Pending.ClientID != next.Pending.ClientID {
		t.Fatal("record missing")
	}
	empty := got
	empty.Pending = nil
	if err = s.SaveSteerAttempt("broker", attemptThread, got, empty); err != nil {
		t.Fatal(err)
	}
	p, _ := s.steerAttemptPath("broker", attemptThread)
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), `"format":1`) || strings.Contains(string(raw), "clientId") {
		t.Fatal("initialized empty record not retained")
	}
	if _, err = os.Stat(p + ".lock"); err != nil {
		t.Fatal("lock outside namespace")
	}
}
func TestSteerAttemptRejectsUnsafeOrUnknown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("daemon steer recovery is not installed on Windows; protected POSIX fixture")
	}
	for _, kind := range []string{"version", "unknown", "oversize", "scope", "symlink", "mode", "lock-link", "directory-link", "hardlink", "parent-mode", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := Open(t.TempDir())
			before, err := s.LoadSteerAttempt("broker", attemptThread)
			if err != nil {
				t.Fatal(err)
			}
			p, _ := s.steerAttemptPath("broker", attemptThread)
			raw, _ := json.Marshal(before)
			switch kind {
			case "version":
				raw = []byte(strings.Replace(string(raw), `"format":1`, `"format":2`, 1))
			case "unknown":
				raw = append(raw[:len(raw)-1], []byte(`,"body":"must-not-be-accepted"}`)...)
			case "oversize":
				raw = []byte(strings.Repeat(" ", 8193))
			case "scope":
				raw = []byte(strings.Replace(string(raw), before.Scope, strings.Repeat("0", 64), 1))
			case "corrupt":
				raw = []byte(`{`)
			}
			if err = os.WriteFile(p, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "hardlink" {
				if err := os.Link(p, p+"-own-link"); err != nil {
					t.Skip("hardlink unavailable")
				}
			}
			if kind == "parent-mode" {
				if runtime.GOOS == "windows" {
					t.Skip("POSIX mode")
				}
				if err := os.Chmod(filepath.Dir(p), 0777); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "mode" {
				if runtime.GOOS == "windows" {
					t.Skip("POSIX mode")
				}
				_ = os.Chmod(p, 0644)
			}
			if kind == "symlink" || kind == "lock-link" || kind == "directory-link" {
				if runtime.GOOS == "windows" {
					t.Skip("symlink privilege")
				}
				target := p
				if kind == "lock-link" {
					target = p + ".lock"
				}
				if kind == "directory-link" {
					target = filepath.Dir(p)
				}
				moved := target + "-own-target"
				if err = os.Rename(target, moved); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(moved, target); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = s.LoadSteerAttempt("broker", attemptThread); !errors.Is(err, ErrSteerRecovery) {
				t.Fatal("unsafe record accepted")
			}
			if err = s.SaveSteerAttempt("broker", attemptThread, before, before); err == nil {
				t.Fatal("corrupt record overwritten")
			}
		})
	}
}

func TestSteerAttemptPruneKeepsBusyRecordAndLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("daemon steer recovery is not installed on Windows; protected POSIX fixture")
	}
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record, err := s.LoadSteerAttempt("scope", attemptThread)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveSteerAttempt("scope", attemptThread, record, record); err != nil {
		t.Fatal(err)
	}
	p, err := s.steerAttemptPath("scope", attemptThread)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p)
	lockBefore, _ := os.Stat(p + ".lock")
	old := time.Now().Add(-365 * 24 * time.Hour)
	os.Chtimes(p, old, old)
	release, err := lockFile(p + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err = s.Prune(time.Now()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(p)
	lockAfter, _ := os.Stat(p + ".lock")
	if err != nil || string(before) != string(after) || !os.SameFile(lockBefore, lockAfter) {
		t.Fatal("prune altered busy attempt record or lock")
	}
	// The 0.17.2 prune iterates only sessions, registrations and diagnostics.
	for _, kind := range []string{"sessions", "registrations", "diagnostics"} {
		if filepath.Dir(p) == filepath.Join(s.Dir(), kind) || filepath.Dir(p+".lock") == filepath.Join(s.Dir(), kind) {
			t.Fatal("old prune selects attempt")
		}
	}
}
