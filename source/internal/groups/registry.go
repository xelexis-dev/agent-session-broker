package groups

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"sync"

	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"

	"github.com/xelexis-dev/agent-session-broker/source/internal/pathguard"
)

var projectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

type directoryPin interface {
	Path() string
	Validate() error
	Close() error
}

type linkFileFunc func(oldPath, newPath string) error

type projectDirectories struct {
	canonicalPath string
	identity      os.FileInfo
	root          directoryPin
}

type Registry struct {
	root          string
	rootDirectory directoryPin
	mu            sync.Mutex
	closed        bool
	projects      map[string]*projectDirectories
}

func NewRegistry(root string) (*Registry, error) {
	return newRegistry(root, pfs.Link)
}

func newRegistry(root string, link linkFileFunc) (*Registry, error) {
	return newRegistryWithCreateHook(root, link, nil)
}

func newRegistryWithCreateHook(root string, link linkFileFunc, hook pathguard.CreateHook) (*Registry, error) {
	resolvedRoot, _, rootDirectory, err := pathguard.EnsureDirectory(root, 0o700, openPathguardPin, hook)
	if err != nil {
		if errors.Is(err, ErrInvalidInput) {
			return nil, sanitizeConstructorError(err)
		}
		return nil, fmt.Errorf("%w: registry root is unsafe", ErrInvalidInput)
	}
	if err := preflightHardLinks(rootDirectory.Path(), link); err != nil {
		_ = rootDirectory.Close()
		return nil, sanitizeConstructorError(err)
	}
	return &Registry{
		root:          resolvedRoot,
		rootDirectory: rootDirectory,
		projects:      make(map[string]*projectDirectories),
	}, nil
}

func openPathguardPin(canonicalPath, operationPath string, identity os.FileInfo) (pathguard.Pin, error) {
	return newDirectoryPin(canonicalPath, operationPath, identity)
}

func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true

	projects := make([]string, 0, len(r.projects))
	for project := range r.projects {
		projects = append(projects, project)
	}
	sort.Strings(projects)
	closeFailed := false
	for _, project := range projects {
		directories := r.projects[project]
		if err := directories.root.Close(); err != nil {
			closeFailed = true
		}
	}
	if err := r.rootDirectory.Close(); err != nil {
		closeFailed = true
	}
	if closeFailed {
		return fmt.Errorf("%w", ErrCorrupt)
	}
	return nil
}

// ProjectStorage creates or opens one encoded project directory through the
// Registry's pinned root and returns a capability that keeps all later child
// storage anchored to that exact directory identity.
func (r *Registry) ProjectStorage(ctx context.Context, project string) (ProjectDirectory, error) {
	if err := validateProject(project); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.readyLocked(ctx); err != nil {
		return nil, err
	}
	directories, _, err := r.projectRootLocked(project, true)
	if err != nil {
		return nil, sanitizeRegistryError(err)
	}
	return ProjectStorage{
		canonicalPath: directories.canonicalPath,
		operationPath: directories.root.Path(),
		validate: func() error {
			return r.validateProjectStorage(project)
		},
	}, nil
}

func (r *Registry) validateProjectStorage(project string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return fmt.Errorf("%w", ErrClosed)
	}
	if err := r.rootDirectory.Validate(); err != nil {
		return fmt.Errorf("%w", ErrCorrupt)
	}
	directories, ok := r.projects[project]
	if !ok || directories.root == nil {
		return fmt.Errorf("%w", ErrCorrupt)
	}
	if err := directories.root.Validate(); err != nil {
		return fmt.Errorf("%w", ErrCorrupt)
	}
	return nil
}

func (r *Registry) readyLocked(ctx context.Context) error {
	if r.closed {
		return fmt.Errorf("%w", ErrClosed)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.rootDirectory.Validate(); err != nil {
		return fmt.Errorf("%w", ErrCorrupt)
	}
	return nil
}

func (r *Registry) projectRootLocked(project string, create bool) (*projectDirectories, bool, error) {
	if directories, ok := r.projects[project]; ok {
		if err := directories.root.Validate(); err != nil {
			return nil, false, fmt.Errorf("%w", ErrCorrupt)
		}
		return directories, true, nil
	}
	encodedProject := projectDirectoryName(project)
	resolvedProject, projectIdentity, directory, exists, err := pathguard.EnsureChild(r.rootDirectory, r.root, encodedProject, create, 0o700, openPathguardPin, nil)
	if err != nil {
		return nil, false, fmt.Errorf("%w", ErrCorrupt)
	}
	if !exists {
		return nil, false, nil
	}
	for existingProject, existing := range r.projects {
		if existingProject != project && os.SameFile(existing.identity, projectIdentity) {
			_ = directory.Close()
			return nil, false, fmt.Errorf("%w", ErrCorrupt)
		}
	}
	directories := &projectDirectories{
		canonicalPath: resolvedProject,
		identity:      projectIdentity,
		root:          directory,
	}
	r.projects[project] = directories
	return directories, true, nil
}

func validateProject(project string) error {
	if !projectPattern.MatchString(project) {
		return fmt.Errorf("%w: invalid project", ErrInvalidInput)
	}
	return nil
}

func projectDirectoryName(project string) string {
	// Never expose a logical project as a path component (for example, "con").
	return "p-" + hex.EncodeToString([]byte(project))
}

func sanitizeRegistryError(err error) error {
	if err == nil {
		return nil
	}
	for _, safe := range []error{
		context.Canceled,
		context.DeadlineExceeded,
		ErrClosed,
		ErrInvalidInput,
		ErrAlreadyRegistered,
		ErrOwnershipConflict,
		ErrSelectorConflict,
		ErrNotFound,
		ErrStorageBusy,
	} {
		if errors.Is(err, safe) {
			return fmt.Errorf("%w", safe)
		}
	}
	return fmt.Errorf("%w", ErrCorrupt)
}

func sanitizeConstructorError(err error) error {
	if errors.Is(err, ErrInvalidInput) {
		return err
	}
	return fmt.Errorf("%w", ErrCorrupt)
}

func preflightHardLinks(directory string, link linkFileFunc) error {
	probe, err := pfs.CreateTemp(directory, ".membership-link-probe-*")
	if err != nil {
		return err
	}
	temporaryPath := probe.Name()
	linkedPath := temporaryPath + ".link"
	defer func() {
		_ = probe.Close()
		_ = pfs.Remove(linkedPath)
		_ = pfs.Remove(temporaryPath)
	}()
	if err := probe.Sync(); err != nil {
		return err
	}
	if err := probe.Close(); err != nil {
		return err
	}
	if err := link(temporaryPath, linkedPath); err != nil {
		return fmt.Errorf("hard-link commit unavailable: %w", err)
	}
	if err := pfs.Remove(linkedPath); err != nil {
		return err
	}
	if err := pfs.Remove(temporaryPath); err != nil {
		return err
	}
	return nil
}
