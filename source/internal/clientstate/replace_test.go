package clientstate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type replaceTarget struct {
	path  string
	old   []byte
	write func() error
	check func()
}

func newReplaceTarget(t *testing.T, kind string) replaceTarget {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal("open fixture")
	}
	key := SessionKey("scope", "codex", "fixture", "client")
	state := SessionState{Registration: Registration{Secret: "synthetic", Revision: 1}}
	if s.SaveSession(key, state) != nil {
		t.Fatal("save fixture")
	}
	f := replaceTarget{path: filepath.Join(s.sessionDir(key), "conn.json")}
	state.Registration.Revision = 2
	f.write = func() error { return s.SaveSession(key, state) }
	f.check = func() {
		got, found, err := s.LoadSession(key)
		if err != nil || !found || got.Registration.Revision != 2 {
			t.Fatal("new session not committed")
		}
	}
	if kind == "handover" {
		if s.MarkHandedOver(key, strings.Repeat("a", 32)) != nil {
			t.Fatal("mark fixture")
		}
		f.path = filepath.Join(s.sessionDir(key), "handover.json")
		f.write = func() error { return s.MarkHandedOver(key, strings.Repeat("b", 32)) }
		f.check = func() {
			got, found, err := s.LoadSession(key)
			if err != nil || !found || got.Registration.HandedOverTo != strings.Repeat("b", 32) {
				t.Fatal("new handover not committed")
			}
		}
	}
	f.old, err = os.ReadFile(f.path)
	if err != nil {
		t.Fatal("read fixture")
	}
	return f
}

func replaceHooks(t *testing.T, blocked error, rename func(string, string) error) {
	t.Helper()
	oldRename, oldBlocked := renameFile, replaceBlocked
	t.Cleanup(func() { renameFile, replaceBlocked = oldRename, oldBlocked })
	renameFile = rename
	replaceBlocked = func(err error) bool { return errors.Is(err, blocked) }
}
func noReplaceTemp(t *testing.T, path string) {
	t.Helper()
	files, err := filepath.Glob(path + ".tmp-*")
	if err != nil || len(files) != 0 {
		t.Fatal("replacement left a temp file")
	}
}
func oldReplaceBytes(t *testing.T, f replaceTarget) {
	t.Helper()
	got, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(got, f.old) {
		t.Fatal("failed replacement changed committed bytes")
	}
	noReplaceTemp(t, f.path)
}

func TestReplaceTransient(t *testing.T) {
	for _, kind := range []string{"session", "handover"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newReplaceTarget(t, kind)
				blocked := errors.New("synthetic blocked rename")
				calls, temporary := 0, ""
				replaceHooks(t, blocked, func(from, to string) error {
					calls++
					if temporary == "" {
						temporary = from
					}
					if from != temporary || to != f.path {
						t.Fatal("retry changed the closed temp or target")
					}
					if calls <= 3 {
						return blocked
					}
					return os.Rename(from, to)
				})
				if f.write() != nil {
					t.Fatal("transient replacement failed")
				}
				if calls != 4 {
					t.Fatalf("rename calls = %d, want 4", calls)
				}
				f.check()
				noReplaceTemp(t, f.path)
			})
		})
	}
}
func TestReplaceFinalError(t *testing.T) {
	for _, kind := range []string{"session", "handover"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newReplaceTarget(t, kind)
				blocked := errors.New("blocked")
				final := errors.New("final")
				calls := 0
				replaceHooks(t, blocked, func(string, string) error { calls++; return final })
				start := time.Now()
				if f.write() == nil {
					t.Fatal("final error accepted")
				}
				if calls != 1 || time.Since(start) != 0 {
					t.Fatal("final error retried")
				}
				oldReplaceBytes(t, f)
			})
		})
	}
}
func TestReplaceBounded(t *testing.T) {
	for _, kind := range []string{"session", "handover"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newReplaceTarget(t, kind)
				blocked := errors.New("blocked")
				calls := 0
				replaceHooks(t, blocked, func(string, string) error { calls++; return blocked })
				start := time.Now()
				if f.write() == nil {
					t.Fatal("permanent obstruction accepted")
				}
				// Strict After retries once at the deadline, then stops on the next step.
				const budget, step = 2 * time.Second, 10 * time.Millisecond
				if time.Since(start) != budget+step || calls != int(budget/step)+2 {
					t.Fatalf("retry elapsed/calls = %s/%d", time.Since(start), calls)
				}
				oldReplaceBytes(t, f)
			})
		})
	}
}
func TestReplaceDiagnosticsSingleAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, err := Open(t.TempDir())
		if err != nil {
			t.Fatal("open fixture")
		}
		blocked := errors.New("blocked")
		calls := 0
		replaceHooks(t, blocked, func(string, string) error { calls++; return blocked })
		key := RegistrationKey("scope", "codex", "11111111-1111-4111-8111-111111111111")
		start := time.Now()
		if s.RecordDiagnostic(key, diagnosticFixture(start)) == nil {
			t.Fatal("blocked diagnostics accepted")
		}
		if calls != 1 || time.Since(start) != 0 {
			t.Fatal("diagnostics entered the retry budget")
		}
		noReplaceTemp(t, filepath.Join(s.Dir(), "diagnostics", "v2", key+".json"))
	})
}
