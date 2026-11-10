//go:build darwin || (linux && !android)

package memory

import (
	"context"
	"errors"
	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
	"os"
	"path/filepath"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
)

func TestNewStoreRejectsAncestorSwapBeforeCreatingMissingRoot(t *testing.T) {
	base := t.TempDir()
	ancestor := filepath.Join(base, "ancestor")
	if err := pfs.Mkdir(ancestor, 0o700); err != nil {
		t.Fatal(err)
	}
	displaced := filepath.Join(base, "displaced")
	outside := t.TempDir()
	swapped := false
	store, err := newStoreWithCreateHook(filepath.Join(ancestor, "missing", "memory"), pfs.Link, func() error {
		if swapped {
			return nil
		}
		swapped = true
		if err := pfs.Rename(ancestor, displaced); err != nil {
			return err
		}
		return os.Symlink(outside, ancestor)
	})
	if store != nil {
		_ = store.Close()
		t.Fatalf("newStoreWithCreateHook(swap) store = %T, want nil", store)
	}
	if !swapped {
		t.Fatal("constructor did not reach guarded create boundary")
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("newStoreWithCreateHook(swap) error = %v, want ErrInvalidInput", err)
	}
	entries, readErr := pfs.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("constructor created through swapped ancestor: %v", entries)
	}
}

func TestNewStoreInProjectRejectsResolveToPinSymlinkSwap(t *testing.T) {
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
	memoryPath := filepath.Join(project.CanonicalPath(), "memory")
	if err := pfs.Mkdir(memoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	displaced := filepath.Join(project.CanonicalPath(), "displaced-memory")
	outside := t.TempDir()
	wrapped := &swapOnValidationProject{
		ProjectDirectory: project,
		swapAt:           2,
		swap: func() error {
			if err := pfs.Rename(memoryPath, displaced); err != nil {
				return err
			}
			return os.Symlink(outside, memoryPath)
		},
	}

	store, err := NewStoreInProject(wrapped)
	if store != nil {
		_ = store.Close()
		t.Fatalf("NewStoreInProject(resolve/pin swap) store = %T, want nil", store)
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("NewStoreInProject(resolve/pin swap) error = %v, want ErrInvalidInput", err)
	}
	entries, err := pfs.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("constructor followed replacement symlink: %v", entries)
	}
}

type swapOnValidationProject struct {
	ProjectDirectory
	validations int
	swapAt      int
	swap        func() error
}

func (p *swapOnValidationProject) Validate() error {
	p.validations++
	if p.validations == p.swapAt {
		if err := p.swap(); err != nil {
			return err
		}
	}
	return p.ProjectDirectory.Validate()
}
