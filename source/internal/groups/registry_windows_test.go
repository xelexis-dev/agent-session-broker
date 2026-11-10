//go:build windows

package groups

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestNewRegistryRejectsJunctionAncestorBeforeCreatingMissingRoot(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	alias := filepath.Join(base, "alias")
	createRegistryDirectoryJunction(t, alias, outside)

	registry, err := NewRegistry(filepath.Join(alias, "missing", "projects"))
	if registry != nil {
		_ = registry.Close()
		t.Fatalf("NewRegistry(ancestor junction) registry = %T, want nil", registry)
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("NewRegistry(ancestor junction) error = %v, want ErrInvalidInput", err)
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("NewRegistry created descendants through ancestor junction: %v", entries)
	}
}

func createRegistryDirectoryJunction(t *testing.T, junction, target string) {
	t.Helper()
	output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junction, target).CombinedOutput()
	if err == nil {
		return
	}
	if errors.Is(err, syscall.Errno(1314)) {
		t.Skipf("junction creation requires unavailable Windows privilege: %v", err)
	}
	t.Fatalf("create directory junction: %v: %s", err, output)
}
