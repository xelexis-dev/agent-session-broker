package clientstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestDeviceSecretConcurrentFirstUseAndMalformedNeverReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x-broker", "device-secret")
	keys := make(chan []byte, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := loadDeviceSecret(t.Context(), path)
			if err != nil {
				keys <- nil
			} else {
				keys <- key
			}
		}()
	}
	wg.Wait()
	close(keys)
	var first []byte
	for key := range keys {
		if len(key) != 32 {
			t.Fatal("first-use failed")
		}
		if first == nil {
			first = key
		}
		if !bytes.Equal(first, key) {
			t.Fatal("first-use generated multiple identities")
		}
	}
	info, err := os.Stat(path)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatal("device secret not private")
	}
	if os.WriteFile(path, []byte("malformed"), 0600) != nil {
		t.Fatal("fixture write")
	}
	if _, err = loadDeviceSecret(t.Context(), path); err == nil {
		t.Fatal("malformed secret replaced")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "malformed" {
		t.Fatal("malformed secret changed")
	}
}
func TestDeviceSecretRejectsSymlinkFileAndAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("reparse checked by Windows seam")
	}
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if os.Mkdir(real, 0700) != nil {
		t.Fatal("fixture")
	}
	alias := filepath.Join(root, "alias")
	if os.Symlink(real, alias) != nil {
		t.Fatal("fixture symlink")
	}
	if _, err := loadDeviceSecret(t.Context(), filepath.Join(alias, "x-broker", "device-secret")); err == nil {
		t.Fatal("symlink ancestor accepted")
	}
	dir := filepath.Join(root, "private")
	if os.Mkdir(dir, 0700) != nil {
		t.Fatal("fixture")
	}
	target := filepath.Join(root, "outside")
	if os.WriteFile(target, bytes.Repeat([]byte("a"), 65), 0600) != nil {
		t.Fatal("fixture")
	}
	if os.Symlink(target, filepath.Join(dir, "device-secret")) != nil {
		t.Fatal("fixture")
	}
	if _, err := loadDeviceSecret(t.Context(), filepath.Join(dir, "device-secret")); err == nil {
		t.Fatal("symlink secret accepted")
	}
}
func TestReceiverIncarnationPIDReuseBootChangeAndProbeUnknown(t *testing.T) {
	key := "boot-a:start-a"
	var probeErr error
	r := NewReceiverIdentity("https://one.invalid/mcp")
	r.secretPath = filepath.Join(t.TempDir(), "x-broker", "device-secret")
	r.pid = func() int { return 100 }
	r.process = func(context.Context, int) (string, error) { return key, probeErr }
	first := r.Current(t.Context())
	if first == nil {
		t.Fatal("identity unavailable")
	}
	challenge := protocol.HolderChallenge{Holder: *first}
	if r.Dead(t.Context(), challenge) {
		t.Fatal("live process considered dead")
	}
	key = "boot-a:start-b"
	if !r.Dead(t.Context(), challenge) {
		t.Fatal("PID reuse not detected")
	}
	key = "boot-b:start-a"
	if !r.Dead(t.Context(), challenge) {
		t.Fatal("reboot not detected")
	}
	probeErr = errors.New("denied")
	if r.Dead(t.Context(), challenge) {
		t.Fatal("unknown probe considered dead")
	}
	probeErr = os.ErrNotExist
	if !r.Dead(t.Context(), challenge) {
		t.Fatal("absent process not detected")
	}
	probeErr = nil
	r.Scope = "https://other.invalid/mcp"
	if r.Dead(t.Context(), challenge) {
		t.Fatal("different server/device considered local")
	}
	next := r.Current(t.Context())
	if next == nil || next.DeviceID == first.DeviceID {
		t.Fatal("device ID not server-scoped")
	}
	r.pid = func() int { return 101 }
	other := r.Current(t.Context())
	if other == nil || other.Incarnation == next.Incarnation {
		t.Fatal("clients under same parent not distinct")
	}
}

func TestDeviceSecretCrossProcessFirstUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x-broker", "device-secret")
	outputs := make(chan []byte, 6)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestDeviceSecretProcessHelper$")
			cmd.Env = append(os.Environ(), "ASB_DEVICE_TEST_PATH="+path)
			out, err := cmd.Output()
			if err != nil {
				outputs <- nil
			} else {
				outputs <- out
			}
		}()
	}
	wg.Wait()
	close(outputs)
	var first []byte
	for out := range outputs {
		if len(out) < 64 {
			t.Fatal("cross-process first use failed")
		}
		if first == nil {
			first = out
		}
		if !bytes.Equal(first, out) {
			t.Fatal("processes observed different device material")
		}
	}
}
func TestDeviceSecretProcessHelper(t *testing.T) {
	path := os.Getenv("ASB_DEVICE_TEST_PATH")
	if path == "" {
		return
	}
	key, err := loadDeviceSecret(t.Context(), path)
	if err != nil || len(key) != 32 {
		os.Exit(71)
	}
	sum := sha256.Sum256(key)
	_, _ = os.Stdout.WriteString(hex.EncodeToString(sum[:]))
}

func loadDeviceSecret(ctx context.Context, path string) ([]byte, error) {
	key, _, err := loadDeviceSecretCause(ctx, path, true)
	return key, err
}
