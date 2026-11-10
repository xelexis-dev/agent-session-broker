//go:build windows

package memory

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
)

func TestProjectStoreRejectsWindowsRootProjectAndMemoryJunctions(t *testing.T) {
	t.Run("supplied root ancestor with missing leaf", func(t *testing.T) {
		parent := t.TempDir()
		outside := t.TempDir()
		alias := filepath.Join(parent, "alias")
		createDirectoryJunction(t, alias, outside)
		store, err := NewStore(filepath.Join(alias, "missing", "memory"))
		if store != nil {
			_ = store.Close()
			t.Fatalf("NewStore(ancestor junction) store = %T, want nil", store)
		}
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("NewStore(ancestor junction) error = %v, want ErrInvalidInput", err)
		}
		entries, readErr := os.ReadDir(outside)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(entries) != 0 {
			t.Fatalf("NewStore created descendants through ancestor junction: %v", entries)
		}
	})

	t.Run("supplied root", func(t *testing.T) {
		parent := t.TempDir()
		outside := t.TempDir()
		root := filepath.Join(parent, "memory")
		createDirectoryJunction(t, root, outside)
		store, err := NewStore(root)
		if store != nil {
			_ = store.Close()
			t.Fatalf("NewStore(junction) store = %T, want nil", store)
		}
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("NewStore(junction) error = %v, want ErrInvalidInput", err)
		}
	})

	t.Run("encoded project", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		createDirectoryJunction(t, filepath.Join(root, "p-6d6573"), outside)
		registry, err := groups.NewRegistry(root)
		if err != nil {
			t.Fatal(err)
		}
		defer registry.Close()
		if _, err := registry.ProjectStorage(context.Background(), "mes"); !errors.Is(err, groups.ErrCorrupt) {
			t.Fatalf("ProjectStorage(junction) error = %v, want ErrCorrupt", err)
		}
	})

	t.Run("cross-project alias", func(t *testing.T) {
		root := t.TempDir()
		registry, err := groups.NewRegistry(root)
		if err != nil {
			t.Fatal(err)
		}
		defer registry.Close()
		mes, err := registry.ProjectStorage(context.Background(), "mes")
		if err != nil {
			t.Fatal(err)
		}
		createDirectoryJunction(t, filepath.Join(root, "p-657270"), mes.CanonicalPath())
		if _, err := registry.ProjectStorage(context.Background(), "erp"); !errors.Is(err, groups.ErrCorrupt) {
			t.Fatalf("ProjectStorage(cross-project junction) error = %v, want ErrCorrupt", err)
		}
	})

	t.Run("project memory", func(t *testing.T) {
		root := t.TempDir()
		registry, err := groups.NewRegistry(filepath.Join(root, "projects"))
		if err != nil {
			t.Fatal(err)
		}
		defer registry.Close()
		project, err := registry.ProjectStorage(context.Background(), "mes")
		if err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		createDirectoryJunction(t, filepath.Join(project.CanonicalPath(), "memory"), outside)
		store, err := NewStoreInProject(project)
		if store != nil {
			_ = store.Close()
			t.Fatalf("NewStoreInProject(junction) store = %T, want nil", store)
		}
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("NewStoreInProject(junction) error = %v, want ErrInvalidInput", err)
		}
	})
}

func createDirectoryJunction(t *testing.T, junction, target string) {
	t.Helper()
	output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junction, target).CombinedOutput()
	if err == nil {
		return
	}
	if errors.Is(err, syscall.Errno(1314)) {
		t.Skipf("junction creation requires unavailable Windows privilege: %v", err)
	}
	t.Fatalf("create directory junction %q -> %q: %v: %s", junction, target, err, output)
}
