package controlpanel

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

type misuse5CrashMeta struct {
	Control, Registrations, URL string
	Proof                       access.DetachedRemovalProof
}

func TestMisuse5Y2KillHelper(t *testing.T) {
	root := os.Getenv("ASB_MISUSE_Y_KILL_ROOT")
	if root == "" {
		t.Skip("subprocess only")
	}
	f := detachedCleanupFixture(t)
	f.detach(t)
	if e := f.remote.Close(); e != nil {
		t.Fatal(e)
	}
	generation := f.store.ProjectGeneration("demo")
	stage := os.Getenv("ASB_MISUSE_Y_KILL_STAGE")
	hook := func(proofs []access.DetachedRemovalProof) error {
		proof := proofs[0]
		if stage == "after-policy" {
			if e := f.store.CleanupDetachedAutoBatch("demo", generation, []access.DetachedRemovalProof{proof}); e != nil {
				return e
			}
		}
		if e := os.WriteFile(filepath.Join(root, "entered"), []byte("hook"), 0600); e != nil {
			return e
		}
		select {}
	}
	var e error
	f.remote, e = broker.NewSQLitePersistentRemoteServiceWithBatchCleanup(f.root, hook)
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e := f.remote.RemoveDetachedSession(r.Context(), f.proof.RegistrationID, nil); e != nil {
			http.Error(w, "cleanup failed", 500)
			return
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	meta, _ := json.Marshal(misuse5CrashMeta{f.store.root.Name(), f.root, server.URL, f.proof})
	// Publish only the complete JSON; the parent polls for the ready name.
	if e = os.WriteFile(filepath.Join(root, "ready.tmp"), meta, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(filepath.Join(root, "ready.tmp"), filepath.Join(root, "ready")); e != nil {
		t.Fatal(e)
	}
	select {}
}
func misuse5WaitFile(t *testing.T, path string) []byte {
	t.Helper()
	until := time.Now().Add(15 * time.Second)
	for time.Now().Before(until) {
		if b, e := os.ReadFile(path); e == nil {
			return b
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child did not reach expected boundary")
	return nil
}
func TestMisuse5Y2KillDuringF1(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("SIGKILL semantics tested on Unix")
	}
	for _, stage := range []string{"before-policy", "after-policy"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			self, e := os.Executable()
			if e != nil {
				t.Fatal(e)
			}
			cmd := exec.Command(self, "-test.run=^TestMisuse5Y2KillHelper$")
			cmd.Env = append(os.Environ(), "ASB_MISUSE_Y_KILL_ROOT="+root, "ASB_MISUSE_Y_KILL_STAGE="+stage, "TMPDIR="+root)
			cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
			if e = cmd.Start(); e != nil {
				t.Fatal(e)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			var meta misuse5CrashMeta
			if json.Unmarshal(misuse5WaitFile(t, filepath.Join(root, "ready")), &meta) != nil {
				t.Fatal("invalid child metadata")
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				client := http.Client{Timeout: 20 * time.Second}
				r, e := client.Get(meta.URL)
				if e == nil {
					r.Body.Close()
				}
			}()
			misuse5WaitFile(t, filepath.Join(root, "entered"))
			if e = cmd.Process.Kill(); e != nil {
				t.Fatal(e)
			}
			if e = cmd.Wait(); e == nil {
				t.Fatal("child was not killed")
			}
			<-done
			store, e := OpenSQLite(meta.Control)
			if e != nil {
				t.Fatal("restart policy", e)
			}
			defer store.Close()
			generation := store.ProjectGeneration("demo")
			audits := func() int {
				n := 0
				for _, a := range store.audits {
					if a.GrantID == meta.Proof.GrantID && a.Reason == "explicit-release" {
						n++
					}
				}
				return n
			}
			expected := 0
			if stage == "after-policy" {
				expected = 1
			}
			if audits() != expected {
				t.Fatal("pre-retry audit count", audits(), expected)
			}
			remote, e := broker.NewSQLitePersistentRemoteServiceWithBatchCleanup(meta.Registrations, func(p []access.DetachedRemovalProof) error {
				return store.CleanupDetachedAutoBatch("demo", generation, p)
			})
			if e != nil {
				t.Fatal(e)
			}
			defer remote.Close()
			if e = remote.RemoveDetachedSession(t.Context(), meta.Proof.RegistrationID, nil); e != nil {
				t.Fatal("retry", e)
			}
			if _, e = remote.GetSession(t.Context(), meta.Proof.RegistrationID); !errors.Is(e, broker.ErrSessionNotFound) {
				t.Fatal("record retained", e)
			}
			if audits() != 1 {
				t.Fatal("lost/duplicate audit", audits())
			}
			for _, token := range store.data.Projects["demo"].Tokens {
				if token.Kind == tokenProject && token.Revoked {
					t.Fatal("parent revoked")
				}
				if token.ID == meta.Proof.GrantID && !token.Revoked {
					t.Fatal("target not revoked")
				}
			}
			t.Log("SIGKILL inside F1 -> restart -> exact grant revoked + registration removed + exactly one audit; parent intact")
		})
	}
}

func TestMisuse5Y3CleanupClockJumps(t *testing.T) {
	for _, delta := range []time.Duration{-10 * time.Minute, 10 * time.Minute} {
		t.Run(delta.String(), func(t *testing.T) {
			f := detachedCleanupFixture(t)
			base := f.store.now()
			f.detach(t)
			f.store.now = func() time.Time { return base.Add(delta) }
			if e := f.remote.RemoveDetachedSession(t.Context(), f.proof.RegistrationID, nil); e != nil {
				t.Fatal(e)
			}
			if !f.revoked() || f.audits() != 1 {
				t.Fatal("clock changed cleanup invariant")
			}
			for _, tok := range f.store.data.Projects["demo"].Tokens {
				if tok.Kind == tokenProject && tok.Revoked {
					t.Fatal("clock revoked parent")
				}
			}
		})
	}
}
