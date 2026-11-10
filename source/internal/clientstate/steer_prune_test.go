//go:build !windows

package clientstate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func pruneAttemptFixture(t *testing.T) (*Store, string, SteerAttemptRecord) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.Dir(), "diagnostics", "v2"), 0700); err != nil {
		t.Fatal(err)
	}
	r, err := s.LoadSteerAttempt("scope", attemptThread)
	if err != nil {
		t.Fatal(err)
	}
	r.Registration, r.Epoch = "R", "E"
	empty := emptySteerRecord("scope", attemptThread)
	if err := s.SaveSteerAttempt("scope", attemptThread, empty, r); err != nil {
		t.Fatal(err)
	}
	p, err := s.steerAttemptPath("scope", attemptThread)
	if err != nil {
		t.Fatal(err)
	}
	return s, p, r
}
func ageAttempt(t *testing.T, p string, now time.Time, age time.Duration) {
	t.Helper()
	old := now.Add(-age)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
}
func TestSteerPruneOldEmptyAndOrphan(t *testing.T) {
	for _, orphan := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "orphan"}[orphan], func(t *testing.T) {
			s, p, _ := pruneAttemptFixture(t)
			now := time.Now()
			if orphan {
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
			} else {
				ageAttempt(t, p, now, 31*24*time.Hour)
			}
			ageAttempt(t, p+".lock", now, 31*24*time.Hour)
			result, err := s.Prune(now)
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{p, p + ".lock"} {
				if _, e := os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
					t.Error("old empty/orphan retained")
				}
			}
			// JSON encoding avoids referring to a result field absent from the RED baseline.
			raw, _ := json.Marshal(result)
			var counts map[string]int
			_ = json.Unmarshal(raw, &counts)
			if counts["SteerAttempts"] != 1 {
				t.Error("missing steer attempt deletion count")
			}
			second, err := s.Prune(now.Add(time.Hour))
			if err != nil || second.Skipped != 1 {
				t.Fatal("daily throttle lost")
			}
		})
	}
}
func TestSteerPruneKeepCases(t *testing.T) {
	for _, kind := range []string{"young", "boundary", "future", "pending", "corrupt", "oversize", "version", "unknown", "trailing", "registration", "epoch", "pair", "digest", "thread", "unexpected", "no-lock", "mode", "hardlink", "lock-mode", "lock-hardlink", "json-link", "lock-link", "parent-link", "namespace-link", "parent-mode"} {
		t.Run(kind, func(t *testing.T) {
			s, p, r := pruneAttemptFixture(t)
			now := time.Now()
			age := 31 * 24 * time.Hour
			switch kind {
			case "young":
				age = 29 * 24 * time.Hour
			case "boundary":
				age = 30 * 24 * time.Hour
			case "future":
				age = -time.Hour
			case "pending":
				r.Pending = &SteerAttempt{ClientID: "xb:R:E:1-2", Sequences: []uint64{1, 2}, Turns: []string{"T"}}
			case "version":
				r.Format = 2
			case "registration":
				r.Registration = "bad:id"
			case "epoch":
				r.Epoch = "bad:id"
			case "pair":
				r.Epoch = ""
			case "digest":
				r.Scope = strings.Repeat("G", 64)
			case "thread":
				r.Thread = "bad-thread"
			}
			raw, _ := json.Marshal(r)
			switch kind {
			case "corrupt":
				raw = []byte("{")
			case "oversize":
				raw = []byte(strings.Repeat(" ", steerAttemptLimit+1))
			case "unknown":
				raw = append(raw[:len(raw)-1], []byte(`,"extra":true}`)...)
			case "trailing":
				raw = append(raw, []byte(`{}`)...)
			}
			if err := os.WriteFile(p, raw, 0600); err != nil {
				t.Fatal(err)
			}
			ageAttempt(t, p, now, age)
			ageAttempt(t, p+".lock", now, 31*24*time.Hour)
			switch kind {
			case "unexpected":
				next := filepath.Join(filepath.Dir(p), "unexpected.json")
				if err := os.Rename(p, next); err != nil {
					t.Fatal(err)
				}
				p = next
			case "no-lock":
				if err := os.Remove(p + ".lock"); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(p, 0644); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(p, p+".copy"); err != nil {
					t.Fatal(err)
				}
			case "lock-mode":
				if err := os.Chmod(p+".lock", 0644); err != nil {
					t.Fatal(err)
				}
			case "lock-hardlink":
				if err := os.Link(p+".lock", p+".lock.copy"); err != nil {
					t.Fatal(err)
				}
			case "json-link", "lock-link", "parent-link", "namespace-link":
				target := p
				if kind == "lock-link" {
					target = p + ".lock"
				}
				if kind == "parent-link" {
					target = filepath.Dir(p)
				}
				if kind == "namespace-link" {
					target = filepath.Dir(filepath.Dir(p))
				}
				moved := target + "-target"
				if err := os.Rename(target, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, target); err != nil {
					t.Fatal(err)
				}
			case "parent-mode":
				if err := os.Chmod(filepath.Dir(p), 0755); err != nil {
					t.Fatal(err)
				}
			}
			result, err := s.Prune(now)
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(p)
			if err != nil || string(after) != string(raw) {
				t.Fatal("retained record changed")
			}
			if result.Skipped == 0 {
				t.Fatal("retention not counted as skipped")
			}
		})
	}
}
func TestSteerPruneHeldLock(t *testing.T) {
	s, p, _ := pruneAttemptFixture(t)
	now := time.Now()
	ageAttempt(t, p, now, 31*24*time.Hour)
	release, err := lockFile(p + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	result, err := s.Prune(now)
	if err != nil || result.Skipped != 1 {
		t.Fatal("held lock not skipped")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("held record removed")
	}
}
func TestSteerPruneOldWaiterFailsClosed(t *testing.T) {
	s, p, _ := pruneAttemptFixture(t)
	oldFile, err := os.Open(p + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer oldFile.Close() // Pin the captured inode against immediate reuse.
	release, err := lockFile(p + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	unlock := func() { releaseOnce.Do(release) }
	defer unlock()
	now := time.Now()
	ageAttempt(t, p, now, 31*24*time.Hour)
	observed, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.steerLockObserved = func() { once.Do(func() { close(observed); <-resume }) }
	done := make(chan error, 1)
	go func() { _, err := s.LoadSteerAttempt("scope", attemptThread); done <- err }()
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("waiter did not snapshot lock")
	}
	released := false
	defer func() {
		if !released {
			close(resume)
		}
	}()
	unlock()
	if _, err := s.Prune(now); err != nil {
		t.Fatal(err)
	}
	writer, err := Open(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	before, err := writer.LoadSteerAttempt("scope", attemptThread)
	if err != nil {
		t.Fatal(err)
	}
	next := before
	next.Registration, next.Epoch = "R", "E"
	next.Pending = &SteerAttempt{ClientID: "xb:R:E:3-3", Sequences: []uint64{3}, Turns: []string{"T"}}
	if err := writer.SaveSteerAttempt("scope", attemptThread, before, next); err != nil {
		t.Fatal(err)
	}
	close(resume)
	released = true
	select {
	case err := <-done:
		if !errors.Is(err, ErrSteerRecovery) {
			t.Fatal("old waiter used replacement lock")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old waiter stuck")
	}
	got, err := s.LoadSteerAttempt("scope", attemptThread)
	if err != nil || !reflect.DeepEqual(got, next) {
		t.Fatal("replacement pending lost")
	}
}

func TestSteerPruneHeldFDRejectsReplacement(t *testing.T) {
	_, p, _ := pruneAttemptFixture(t)
	for _, path := range []string{p, p + ".lock"} {
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := samePrivateSteerFile(path, file); !ok {
			t.Fatal("fixture inode mismatch")
		}
		if err := os.Rename(path, path+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte{}, 0600); err != nil {
			t.Fatal(err)
		}
		_, ok := samePrivateSteerFile(path, file)
		file.Close()
		if ok {
			t.Fatal("held fd accepted replacement path")
		}
	}
}
func TestSteerPruneScopeFreeAndOrphanAge(t *testing.T) {
	t.Run("scope-free", func(t *testing.T) {
		s, p, r := pruneAttemptFixture(t)
		r.Scope = strings.Repeat("0", 64)
		raw, _ := json.Marshal(r)
		if err := os.WriteFile(p, raw, 0600); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		ageAttempt(t, p, now, 31*24*time.Hour)
		result, err := s.Prune(now)
		if err != nil || result.SteerAttempts != 1 {
			t.Fatal("pruner incorrectly bound filename to raw scope")
		}
	})
	for _, age := range []time.Duration{29 * 24 * time.Hour, 30 * 24 * time.Hour, -time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			s, p, _ := pruneAttemptFixture(t)
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			ageAttempt(t, p+".lock", now, age)
			result, err := s.Prune(now)
			if err != nil || result.SteerAttempts != 0 || result.Skipped != 1 {
				t.Fatal("young orphan not kept")
			}
			if _, err := os.Stat(p + ".lock"); err != nil {
				t.Fatal("young orphan removed")
			}
		})
	}
}
func TestSteerPruneFailedJSONUnlinkKeepsLock(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write mode")
	}
	s, p, _ := pruneAttemptFixture(t)
	now := time.Now()
	ageAttempt(t, p, now, 31*24*time.Hour)
	if err := os.Chmod(filepath.Dir(p), 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Dir(p), 0700)
	result, err := s.Prune(now)
	if err == nil || result.Errors != 1 || result.SteerAttempts != 0 {
		t.Fatal("JSON unlink failure not reported")
	}
	for _, path := range []string{p, p + ".lock"} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("unlink failure removed lock or record")
		}
	}
}
