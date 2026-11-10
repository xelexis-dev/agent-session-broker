package mcpserver

import (
	"context"
	"errors"

	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
)

// NewManagedProjectFactory lets the server select durable project storage while
// compatibility callers retain their existing in-memory remote services.
func NewManagedProjectFactory(registry ProjectRegistry, newRemote func(groups.ProjectDirectory) (Backend, error), version ...string) *ProjectFactory {
	f := NewRemoteProjectFactory(registry, version...)
	if newRemote == nil {
		panic("managed project factory requires remote backend constructor")
	}
	f.newRemote = newRemote
	return f
}

type remoteProjectStore struct {
	projectMemoryStore
	backend Backend
}

// CloseProject evicts a managed project's stores after the HTTP owner has
// stopped admission and drained its requests. Unmanaged project state
// must never be evicted using this method.
func (f *ProjectFactory) CloseProject(ctx context.Context, project string) error {
	if !projectPattern.MatchString(project) {
		return ErrInvalidProject
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.closed {
		return ErrProjectFactoryClosed
	}
	if f.newRemote == nil {
		return errors.New("project is not managed")
	}
	if f.initializing[project] != nil {
		return errors.New("project initialization still active")
	}
	if entry, ok := f.projects[project]; ok {
		if err := entry.store.Close(); err != nil {
			return err
		}
		delete(f.projects, project)
	}
	delete(f.admitted, project)
	return nil
}

func (s *remoteProjectStore) Close() error {
	var err error
	if closer, ok := s.backend.(interface{ Close() error }); ok {
		err = closer.Close()
	}
	return errors.Join(err, s.projectMemoryStore.Close())
}

// RemoteBackend returns the same project service used by the MCP transport.
func (f *ProjectFactory) RemoteBackend(ctx context.Context, project string) (Backend, error) {
	if _, err := f.Server(ctx, project); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, ErrProjectFactoryClosed
	}
	entry, ok := f.projects[project]
	if !ok {
		return nil, errors.New("project unavailable")
	}
	remote, ok := entry.store.(*remoteProjectStore)
	if !ok {
		return nil, errors.New("project is not remote")
	}
	return remote.backend, nil
}
