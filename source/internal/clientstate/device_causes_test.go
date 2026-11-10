//go:build darwin || linux

package clientstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func receiverCauseFixture(t *testing.T) *ReceiverIdentity {
	t.Helper()
	r := NewReceiverIdentity("fixture")
	r.secretPath = filepath.Join(t.TempDir(), "private", "device-secret")
	r.pid = func() int { return 100 }
	r.process = func(context.Context, int) (string, error) { return "fixture-process", nil }
	return r
}
func assertDeviceCause(t *testing.T, r *ReceiverIdentity, want string) {
	t.Helper()
	if r.Current(t.Context()) != nil || r.DiagnosticCause() != want {
		t.Fatal("unexpected device cause: want", want, "got", r.DiagnosticCause())
	}
}
func TestDeviceIdentityCauseFixtures(t *testing.T) {
	t.Run("path-unavailable", func(t *testing.T) {
		r := receiverCauseFixture(t)
		r.secretPath = "relative/device-secret"
		assertDeviceCause(t, r, "path-unavailable")
	})
	t.Run("directory-insecure", func(t *testing.T) {
		r := receiverCauseFixture(t)
		os.MkdirAll(filepath.Dir(r.secretPath), 0755)
		os.Chmod(filepath.Dir(r.secretPath), 0755)
		assertDeviceCause(t, r, "directory-insecure")
	})
	t.Run("directory-link", func(t *testing.T) {
		r := receiverCauseFixture(t)
		target := t.TempDir()
		os.Symlink(target, filepath.Dir(r.secretPath))
		assertDeviceCause(t, r, "directory-insecure")
	})
	t.Run("directory-unwritable", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("permission fixture needs unprivileged user")
		}
		r := receiverCauseFixture(t)
		parent := filepath.Join(t.TempDir(), "readonly")
		os.Mkdir(parent, 0500)
		defer os.Chmod(parent, 0700)
		r.secretPath = filepath.Join(parent, "child", "device-secret")
		assertDeviceCause(t, r, "directory-unwritable")
	})
	t.Run("lock-failed", func(t *testing.T) {
		r := receiverCauseFixture(t)
		os.MkdirAll(filepath.Dir(r.secretPath), 0700)
		os.WriteFile(r.secretPath+".lock", nil, 0644)
		assertDeviceCause(t, r, "lock-failed")
	})
	for _, kind := range []string{"mode", "link", "hardlink", "directory"} {
		t.Run("secret-insecure-"+kind, func(t *testing.T) {
			r := receiverCauseFixture(t)
			if r.Current(t.Context()) == nil {
				t.Fatal("fixture identity")
			}
			switch kind {
			case "mode":
				os.Chmod(r.secretPath, 0644)
			case "link":
				os.Remove(r.secretPath)
				target := filepath.Join(t.TempDir(), "target")
				os.WriteFile(target, []byte(strings.Repeat("a", 64)+"\n"), 0600)
				os.Symlink(target, r.secretPath)
			case "hardlink":
				os.Link(r.secretPath, r.secretPath+".copy")
			case "directory":
				os.Remove(r.secretPath)
				os.Mkdir(r.secretPath, 0700)
			}
			assertDeviceCause(t, r, "secret-insecure")
		})
	}
	for _, raw := range []string{"short", strings.Repeat("g", 64) + "\n", strings.Repeat("a", 65)} {
		t.Run("secret-malformed", func(t *testing.T) {
			r := receiverCauseFixture(t)
			if r.Current(t.Context()) == nil {
				t.Fatal("fixture identity")
			}
			os.WriteFile(r.secretPath, []byte(raw), 0600)
			assertDeviceCause(t, r, "secret-malformed")
		})
	}
	t.Run("secret-missing", func(t *testing.T) {
		r := receiverCauseFixture(t)
		if r.Current(t.Context()) == nil {
			t.Fatal("fixture identity")
		}
		os.Remove(r.secretPath)
		assertDeviceCause(t, r, "secret-missing")
		if _, err := os.Stat(r.secretPath); !os.IsNotExist(err) {
			t.Fatal("running process regenerated secret")
		}
		fresh := receiverCauseFixture(t)
		fresh.secretPath = r.secretPath
		if fresh.Current(t.Context()) == nil || fresh.DiagnosticCause() != "secret-created-lock-present" {
			t.Fatal("new process behavior changed")
		}
	})
	t.Run("secret-changed", func(t *testing.T) {
		r := receiverCauseFixture(t)
		if r.Current(t.Context()) == nil {
			t.Fatal("fixture identity")
		}
		os.WriteFile(r.secretPath, []byte(strings.Repeat("b", 64)+"\n"), 0600)
		assertDeviceCause(t, r, "secret-changed")
	})
	t.Run("process-identity-unavailable", func(t *testing.T) {
		r := receiverCauseFixture(t)
		r.process = func(context.Context, int) (string, error) { return "", errors.New("private-error-sentinel") }
		assertDeviceCause(t, r, "process-identity-unavailable")
	})
}

// The second Done call occurs at the real OS-lock retry after local serialization.
type waitingDeviceContext struct {
	context.Context
	calls   atomic.Int32
	waiting chan struct{}
}

func (c *waitingDeviceContext) Done() <-chan struct{} {
	if c.calls.Add(1) == 2 {
		close(c.waiting)
	}
	return c.Context.Done()
}
func TestDeviceSecretCreateFailureCause(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permission fixture needs unprivileged user")
	}
	r := receiverCauseFixture(t)
	dir := filepath.Dir(r.secretPath)
	os.MkdirAll(dir, 0700)
	f, err := os.OpenFile(r.secretPath+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal("lock fixture")
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal("lock fixture")
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	base, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx := &waitingDeviceContext{Context: base, waiting: make(chan struct{})}
	result := make(chan string, 1)
	go func() { _, cause, _ := loadDeviceSecretCause(ctx, r.secretPath, true); result <- cause }()
	select {
	case <-ctx.waiting:
	case <-base.Done():
		t.Fatal("loader did not reach OS lock")
	}
	os.Chmod(dir, 0500)
	defer os.Chmod(dir, 0700)
	unix.Flock(int(f.Fd()), unix.LOCK_UN)
	if cause := <-result; cause != "secret-create-failed" {
		t.Fatal("temporary file failure misclassified", cause)
	}
}

func TestDeviceSecretCreationObservationAndCancellation(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run("creation", func(t *testing.T) {
			r := receiverCauseFixture(t)
			if existing {
				os.MkdirAll(filepath.Dir(r.secretPath), 0700)
				os.WriteFile(r.secretPath+".lock", nil, 0600)
			}
			want := "secret-created"
			if existing {
				want = "secret-created-lock-present"
			}
			if r.Current(t.Context()) == nil || r.DiagnosticCause() != want {
				t.Fatal("creation observation incorrect")
			}
			if r.Current(t.Context()) == nil || r.DiagnosticCause() != want {
				t.Fatal("creation observation disappeared on reread")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			r.Current(ctx)
			if r.DiagnosticCause() != want {
				t.Fatal("cancelled probe replaced previous cause")
			}
			entries, _ := os.ReadDir(filepath.Dir(r.secretPath))
			if len(entries) != 2 {
				t.Fatal("unexpected marker or temp file added")
			}
		})
	}
}

func TestRound4DeviceLockSymlink(t *testing.T) {
	r := receiverCauseFixture(t)
	if r.Current(t.Context()) == nil {
		t.Fatal("fixture identity")
	}
	external := filepath.Join(t.TempDir(), "outside-lock")
	raw := []byte("private-lock-sentinel")
	if os.WriteFile(external, raw, 0600) != nil || os.Remove(r.secretPath+".lock") != nil || os.Symlink(external, r.secretPath+".lock") != nil {
		t.Fatal("fixture lock symlink")
	}
	assertDeviceCause(t, r, "lock-failed")
	after, err := os.ReadFile(external)
	if err != nil || string(after) != string(raw) {
		t.Fatal("external lock changed")
	}
}
