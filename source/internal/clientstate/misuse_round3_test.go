package clientstate

import (
	"context"
	"encoding/hex"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestRound3DeviceSecretMutationNeverReplacesRunningIdentity(t *testing.T) {
	for _, mutation := range []string{"deleted", "symlink", "permissions", "corrupt", "replacement", "parent-permissions"} {
		t.Run(mutation, func(t *testing.T) {
			if runtime.GOOS == "windows" && mutation == "symlink" {
				t.Skip("symlink requires Windows privilege")
			}
			r := NewReceiverIdentity("https://fixture.invalid/mcp")
			r.secretPath = filepath.Join(t.TempDir(), "private", "device-secret")
			r.process = func(context.Context, int) (string, error) { return "fixture-boot:start", nil }
			before := r.Current(t.Context())
			if before == nil {
				t.Fatal("fixture identity unavailable")
			}
			original, err := os.ReadFile(r.secretPath)
			if err != nil {
				t.Fatal("fixture original")
			}
			restore := func() {}
			switch mutation {
			case "deleted":
				if err := os.Remove(r.secretPath); err != nil {
					t.Fatal("fixture delete")
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "private", "device-secret")
				if _, err := loadDeviceSecret(t.Context(), target); err != nil {
					t.Fatal("fixture target")
				}
				if os.Remove(r.secretPath) != nil || os.Symlink(target, r.secretPath) != nil {
					t.Fatal("fixture symlink")
				}
			case "permissions":
				restore = insecureDevicePath(t, r.secretPath)
			case "corrupt":
				if os.WriteFile(r.secretPath, []byte("corrupt"), 0600) != nil {
					t.Fatal("fixture corrupt")
				}
			case "replacement":
				other := filepath.Join(t.TempDir(), "private", "device-secret")
				key, err := loadDeviceSecret(t.Context(), other)
				if err != nil {
					t.Fatal("fixture replacement")
				}
				if os.WriteFile(r.secretPath, []byte(hex.EncodeToString(key)+"\n"), 0600) != nil {
					t.Fatal("fixture replace")
				}
			case "parent-permissions":
				restore = insecureDevicePath(t, filepath.Dir(r.secretPath))
			}
			if r.Current(t.Context()) != nil {
				t.Fatal("running receiver silently accepted/recreated altered device secret")
			}
			wantCause := map[string]string{"deleted": "secret-missing", "symlink": "secret-insecure", "permissions": "secret-insecure", "corrupt": "secret-malformed", "replacement": "secret-changed", "parent-permissions": "directory-insecure"}[mutation]
			if r.DiagnosticCause() != wantCause {
				t.Fatal("mutation cause mismatch", mutation, r.DiagnosticCause())
			}
			if r.Dead(t.Context(), protocol.HolderChallenge{Holder: *before}) {
				t.Fatal("unavailable secret produced a death proof")
			}
			if mutation == "deleted" {
				if _, err := os.Lstat(r.secretPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("deleted secret was recreated")
				}
			}
			// Restoring the same material restores availability; no terminal latch is introduced.
			restore()
			if os.Chmod(filepath.Dir(r.secretPath), 0700) != nil {
				t.Fatal("fixture restore permissions")
			}
			if mutation != "deleted" {
				if os.Remove(r.secretPath) != nil {
					t.Fatal("fixture remove mutation")
				}
			}
			if runtime.GOOS == "windows" {
				// Recreate with the private DACL before restoring the original material.
				if _, err := loadDeviceSecret(t.Context(), r.secretPath); err != nil {
					t.Fatal("fixture restore DACL")
				}
			}
			if os.WriteFile(r.secretPath, original, 0600) != nil {
				t.Fatal("fixture restore")
			}
			if current := r.Current(t.Context()); current == nil || *current != *before {
				t.Fatal("restored secret did not restore original identity")
			}

		})
	}
}

func TestRound3ReceiverProbeTimeoutIsNotDeath(t *testing.T) {
	r := NewReceiverIdentity("fixture")
	r.secretPath = filepath.Join(t.TempDir(), "private", "device-secret")
	r.process = func(context.Context, int) (string, error) { return "start", nil }
	holder := r.Current(t.Context())
	if holder == nil {
		t.Fatal("fixture identity")
	}
	r.process = func(context.Context, int) (string, error) { return "", context.DeadlineExceeded }
	if r.Dead(t.Context(), protocol.HolderChallenge{Holder: *holder}) {
		t.Fatal("timeout was treated as death")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if r.Dead(ctx, protocol.HolderChallenge{Holder: *holder}) {
		t.Fatal("cancellation was treated as death")
	}
}

func TestRound3TwoVersionStoresKeepSeparateSessionsAndDiagnostics(t *testing.T) {
	a, err := Open(t.TempDir())
	if err != nil {
		t.Fatal("fixture state")
	}
	b, err := Open(a.Dir())
	if err != nil {
		t.Fatal("second process state")
	}
	// SessionState/Registration codecs are unchanged from tag 0.17.1.
	var wg sync.WaitGroup
	for i, s := range []*Store{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := SessionKey("fixture", "claude", []string{"a", "b"}[i], "client")
			state := SessionState{Scope: "fixture", Harness: "claude", SessionID: []string{"a", "b"}[i], ClientID: "client", Registration: Registration{Secret: "synthetic-proof"}}
			for n := 0; n < 10; n++ {
				if s.SaveSession(key, state) != nil {
					t.Error("mixed-version state write")
				}
				if s.RecordDiagnostic(key, diagnosticFixture(time.Now().UTC())) != nil {
					t.Error("mixed-version diagnostic write")
				}
			}
			loaded, ok, err := s.LoadSession(key)
			if err != nil || !ok || loaded.SessionID != state.SessionID {
				t.Error("mixed-version session corrupted")
			}
			if _, err := s.LoadDiagnostics(key); err != nil {
				t.Error("mixed-version diagnostics corrupted")
			}
		}()
	}
	wg.Wait()
}
