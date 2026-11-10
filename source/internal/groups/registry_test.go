//go:build (windows || linux || darwin) && !android

package groups

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
)

func TestRegistryRejectsNoncanonicalProjects(t *testing.T) {
	registry := mustRegistry(t, t.TempDir())
	for _, project := range []string{"", "MES", " mes", "mes ", "../mes", "mes/team", ".mes", strings.Repeat("a", 65)} {
		t.Run(project, func(t *testing.T) {
			if _, err := registry.ProjectStorage(t.Context(), project); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("ProjectStorage(%q)=%v", project, err)
			}
		})
	}
	for _, project := range []string{"a", "mes.api-2", strings.Repeat("z", 64)} {
		t.Run("valid-"+project, func(t *testing.T) {
			directory, err := registry.ProjectStorage(t.Context(), project)
			if err != nil {
				t.Fatal(err)
			}
			if err = directory.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNewRegistryRejectsSymlinkAncestorBeforeCreatingMissingRoot(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(outside, alias); err != nil {
		skipIfSymlinkPrivilegeUnavailable(t, err)
		t.Fatalf("create ancestor symlink: %v", err)
	}

	registry, err := NewRegistry(filepath.Join(alias, "missing", "projects"))
	if registry != nil {
		_ = registry.Close()
		t.Fatalf("NewRegistry(ancestor symlink) registry = %T, want nil", registry)
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("NewRegistry(ancestor symlink) error = %v, want ErrInvalidInput", err)
	}
	entries, readErr := pfs.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("NewRegistry created descendants through ancestor symlink: %v", entries)
	}
}

func TestProjectStoragePinsCanonicalEncodedProject(t *testing.T) {
	root := t.TempDir()
	registry := mustRegistry(t, root)

	storage, err := registry.ProjectStorage(context.Background(), "mes")
	if err != nil {
		t.Fatalf("ProjectStorage() error = %v", err)
	}
	canonicalRoot, err := pfs.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(canonicalRoot, "p-6d6573")
	if storage.CanonicalPath() != want {
		t.Fatalf("ProjectStorage().CanonicalPath() = %q, want %q", storage.CanonicalPath(), want)
	}
	if storage.OperationPath() == "" {
		t.Fatal("ProjectStorage().OperationPath() is empty")
	}
	if err := storage.Validate(); err != nil {
		t.Fatalf("ProjectStorage().Validate() error = %v", err)
	}
	info, err := pfs.Stat(storage.OperationPath())
	if err != nil || !info.IsDir() {
		t.Fatalf("ProjectStorage().OperationPath() is not an accessible directory: (%v, %v)", info, err)
	}
}

func TestProjectStorageRejectsCrossProjectSymlinkAlias(t *testing.T) {
	root := t.TempDir()
	registry := mustRegistry(t, root)
	mes, err := registry.ProjectStorage(context.Background(), "mes")
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "p-657270")
	if err := os.Symlink(mes.CanonicalPath(), alias); err != nil {
		skipIfSymlinkPrivilegeUnavailable(t, err)
		t.Fatalf("create cross-project alias: %v", err)
	}

	if _, err := registry.ProjectStorage(context.Background(), "erp"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ProjectStorage(alias) error = %v, want ErrCorrupt", err)
	}
}

func TestRegistryRejectsRenamedCachedProjectDirectory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Windows pins block ancestor rename; unsupported platforms fail at construction")
	}
	root := t.TempDir()
	registry := mustRegistry(t, root)
	if _, err := registry.ProjectStorage(context.Background(), "mes"); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "p-6d6573")
	moved := filepath.Join(root, "moved-project")
	if err := pfs.Rename(project, moved); err != nil {
		t.Fatal(err)
	}
	if err := pfs.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}

	_, err := registry.ProjectStorage(context.Background(), "mes")
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ProjectStorage() after project rename error = %v, want ErrCorrupt", err)
	}
}

func TestRegistryRejectsRenamedRootDirectory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Windows pins block root rename; unsupported platforms fail at construction")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "registry")
	registry := mustRegistry(t, root)
	if _, err := registry.ProjectStorage(context.Background(), "mes"); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "moved-registry")
	if err := pfs.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := pfs.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := registry.ProjectStorage(context.Background(), "mes"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ProjectStorage() after root rename error = %v, want ErrCorrupt", err)
	}
}

func TestRegistryRejectsSymlinkedProjectAndDoesNotEscapeRoot(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("registry storage is unsupported on this platform")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "p-6d6573")); err != nil {
		skipIfSymlinkPrivilegeUnavailable(t, err)
		t.Fatalf("create project symlink: %v", err)
	}
	registry := mustRegistry(t, root)
	_, err := registry.ProjectStorage(context.Background(), "mes")
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ProjectStorage() through project symlink error = %v, want ErrCorrupt", err)
	}
	entries, readErr := pfs.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("registry wrote outside root: %v", entries)
	}
}

func TestRegistryCloseIsIdempotentAndStopsOperations(t *testing.T) {
	registry := mustRegistry(t, t.TempDir())
	if err := registry.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := registry.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if _, err := registry.ProjectStorage(context.Background(), "mes"); !errors.Is(err, ErrClosed) {
		t.Fatalf("ProjectStorage() after Close error = %v, want ErrClosed", err)
	}
}

func TestRegistryCloseDoesNotRevealProjectOnHandleFailure(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "secret-project", "sessions")
	registry := &Registry{
		rootDirectory: inertDirectoryPin{},
		projects: map[string]*projectDirectories{
			"secret-project": {root: inertDirectoryPin{closeErr: &os.PathError{Op: "close", Path: secretPath, Err: os.ErrPermission}}},
		},
	}
	err := registry.Close()
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Close() error = %v, want ErrCorrupt", err)
	}
	for _, forbidden := range []string{"secret-project", secretPath} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("Close() error reveals %q: %v", forbidden, err)
		}
	}
}

func TestNewRegistrySanitizesPreflightPathError(t *testing.T) {
	root := t.TempDir()
	secretPath := filepath.Join(root, "secret-preflight")
	registry, err := newRegistry(root, func(_, _ string) error {
		return &os.LinkError{Op: "link", Old: secretPath, New: secretPath + ".link", Err: os.ErrPermission}
	})
	if registry != nil || !errors.Is(err, ErrCorrupt) {
		t.Fatalf("newRegistry() = (%T, %v), want nil ErrCorrupt", registry, err)
	}
	if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "secret-preflight") {
		t.Fatalf("newRegistry() error leaks path: %v", err)
	}
}

func TestRegistrySanitizesPinnedValidationPathError(t *testing.T) {
	root := t.TempDir()
	registry := mustRegistry(t, root)
	secretPath := filepath.Join(root, "secret-validation")
	registry.rootDirectory = failingDirectoryPin{
		directoryPin: registry.rootDirectory,
		validateErr:  &os.PathError{Op: "stat", Path: secretPath, Err: os.ErrPermission},
	}

	_, err := registry.ProjectStorage(context.Background(), "mes")
	if !errors.Is(err, ErrCorrupt) || strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "secret-validation") {
		t.Fatalf("ProjectStorage() validation error = %v, want path-free ErrCorrupt", err)
	}
}

func TestRegistryHonorsCanceledContext(t *testing.T) {
	registry := mustRegistry(t, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := registry.ProjectStorage(ctx, "mes"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ProjectStorage(canceled) error = %v, want context.Canceled", err)
	}
}

func mustRegistry(t *testing.T, root string) *Registry {
	t.Helper()
	registry, err := NewRegistry(root)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	t.Cleanup(func() {
		if err := registry.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return registry
}

func skipIfSymlinkPrivilegeUnavailable(t *testing.T, err error) {
	t.Helper()
	if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(1314)) {
		t.Skipf("symlink creation requires unavailable Windows privilege: %v", err)
	}
}

type inertDirectoryPin struct {
	closeErr error
}

func (inertDirectoryPin) Path() string     { return "" }
func (inertDirectoryPin) Validate() error  { return nil }
func (pin inertDirectoryPin) Close() error { return pin.closeErr }

type failingDirectoryPin struct {
	directoryPin
	validateErr error
}

func (pin failingDirectoryPin) Validate() error { return pin.validateErr }
