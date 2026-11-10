//go:build darwin || (linux && !android)

package groups

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
)

func TestNewRegistryRejectsAncestorSwapBeforeCreatingMissingRoot(t *testing.T) {
	base := t.TempDir()
	ancestor := filepath.Join(base, "ancestor")
	if err := pfs.Mkdir(ancestor, 0o700); err != nil {
		t.Fatal(err)
	}
	displaced := filepath.Join(base, "displaced")
	outside := t.TempDir()
	swapped := false
	registry, err := newRegistryWithCreateHook(filepath.Join(ancestor, "missing", "projects"), pfs.Link, func() error {
		if swapped {
			return nil
		}
		swapped = true
		if err := pfs.Rename(ancestor, displaced); err != nil {
			return err
		}
		return os.Symlink(outside, ancestor)
	})
	if registry != nil {
		_ = registry.Close()
		t.Fatalf("newRegistryWithCreateHook(swap) registry = %T, want nil", registry)
	}
	if !swapped {
		t.Fatal("constructor did not reach guarded create boundary")
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("newRegistryWithCreateHook(swap) error = %v, want ErrInvalidInput", err)
	}
	entries, readErr := pfs.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("constructor created through swapped ancestor: %v", entries)
	}
}
