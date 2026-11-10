package groups

import (
	"context"
	"errors"
	"os"
)

// DeleteProjectStorage requires the caller to drain all project operations and
// close their stores first. The encoded child is removed relative to the pinned
// root; nested links are unlinked rather than traversed by os.Root.RemoveAll.
func (r *Registry) DeleteProjectStorage(ctx context.Context, project string) error {
	if err := validateProject(project); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.readyLocked(ctx); err != nil {
		return err
	}
	if err := r.rootDirectory.Validate(); err != nil {
		return ErrCorrupt
	}
	root, err := os.OpenRoot(r.rootDirectory.Path())
	if err != nil {
		return ErrCorrupt
	}
	defer root.Close()
	if err = r.rootDirectory.Validate(); err != nil {
		return ErrCorrupt
	}
	name := projectDirectoryName(project)
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrCorrupt
	}
	if dirs := r.projects[project]; dirs != nil {
		if !os.SameFile(info, dirs.identity) || dirs.root.Validate() != nil {
			return ErrCorrupt
		}
		closeErr := dirs.root.Close()
		delete(r.projects, project)
		if closeErr != nil {
			return ErrCorrupt
		}
	}
	if err = root.RemoveAll(name); err != nil {
		return ErrCorrupt
	}
	if err = r.rootDirectory.Validate(); err != nil {
		return ErrCorrupt
	}
	return nil
}
