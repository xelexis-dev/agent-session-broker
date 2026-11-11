package controlpanel

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"sync"
)

type projectOperations struct {
	count int
	idle  chan struct{}
}

// startProjectOperation fences backend calls against deletion. For MCP the
// generation must still be the one that authorized the request.
func (s *Store) startProjectOperation(name string, generation <-chan struct{}) (func(), error) {
	return s.startProjectOperationGuarded(name, generation, nil)
}
func (s *Store) startProjectOperationGuarded(name string, generation <-chan struct{}, guard commitGuard) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data.Projects[name]
	if s.closed || !ok || p.Deleting || (generation != nil && s.changes[name] != generation) {
		return nil, ErrForbidden
	}
	if err := guard.check(); err != nil {
		return nil, err
	}
	if s.operations == nil {
		s.operations = map[string]*projectOperations{}
	}
	op := s.operations[name]
	if op == nil {
		op = &projectOperations{idle: make(chan struct{})}
		s.operations[name] = op
	}
	op.count++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			op.count--
			if op.count == 0 {
				close(op.idle)
				delete(s.operations, name)
			}
		})
	}, nil
}

func (s *Store) beginProjectDeletion(actor, name, confirmation string) (<-chan struct{}, error) {
	return s.beginProjectDeletionGuarded(actor, name, confirmation, "", nil)
}
func (s *Store) beginProjectDeletionGuarded(actor, name, confirmation, expectedGeneration string, guard commitGuard) (<-chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.projectRoleLocked(actor, name) != "owner" {
		return nil, ErrForbidden
	}
	if confirmation != name {
		return nil, ErrInvalid
	}
	p := s.data.Projects[name]
	if expectedGeneration != "" && p.ProjectGenerationID != expectedGeneration {
		return nil, errProjectGenerationMismatch
	}
	if err := guard.check(); err != nil {
		return nil, err
	}
	if !p.Deleting {
		p.Deleting = true
		p.Enabled = false
		p.Tokens = nil
		p.Devices = nil
		p.UpdatedAt = s.now()
		candidate := s.data
		candidate.Projects = maps.Clone(s.data.Projects)
		candidate.Projects[name] = p
		if err := s.commit(candidate); err != nil {
			return nil, err
		}
		s.changed(name)
	}
	if op := s.operations[name]; op != nil {
		return op.idle, nil
	}
	idle := make(chan struct{})
	close(idle)
	return idle, nil
}

func (s *Store) finishProjectDeletion(name string, expectedGeneration ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data.Projects[name]
	if !ok || !p.Deleting || (len(expectedGeneration) > 0 && (len(expectedGeneration) != 1 || expectedGeneration[0] != p.ProjectGenerationID)) {
		return ErrInvalid
	}
	if op := s.operations[name]; op != nil {
		return ErrInvalid
	}
	candidate := s.data
	candidate.Projects = maps.Clone(s.data.Projects)
	delete(candidate.Projects, name)
	return s.commit(candidate)
}

type projectDeleter interface {
	DeleteProject(context.Context, string) error
}

func (p *panel) deleteProject(w http.ResponseWriter, r *http.Request, session loginSession) {
	name := r.PostForm.Get("project")
	// Reject ambiguous or unexpected fields instead of silently choosing one.
	for k, v := range r.PostForm {
		if (k != "csrf" && k != "project" && k != "confirmName") || len(v) != 1 {
			http.Error(w, "invalid form", 400)
			return
		}
	}
	err := p.deleteProjectAs(r.Context(), session.userID, name, r.PostForm.Get("confirmName"), "", nil)
	switch {
	case err == nil:
		p.redirectNotice(w, r, "/", "project-deleted")
	case errors.Is(err, errProjectDeleteUnavailable):
		http.Error(w, "project deletion unavailable", http.StatusServiceUnavailable)
	case errors.Is(err, errProjectDeleteInterrupted):
		http.Error(w, "project deletion interrupted", http.StatusServiceUnavailable)
	case errors.Is(err, errProjectDeletePending):
		p.show(w, r, session, "settings", name, 503, "projects.error_delete_pending", "")
	case errors.Is(err, ErrForbidden):
		http.Error(w, "forbidden", http.StatusForbidden)
	case errors.Is(err, ErrInvalid):
		p.show(w, r, session, "settings", name, 400, "projects.error_confirm_name", "")
	default:
		http.Error(w, "project deletion unavailable", http.StatusServiceUnavailable)
	}
}

var errProjectDeleteUnavailable = errors.New("project deletion unavailable")
var errProjectDeleteInterrupted = errors.New("project deletion interrupted")
var errProjectDeletePending = errors.New("project deletion pending")

func (p *panel) deleteProjectAs(ctx context.Context, actor, name, confirmation, expectedGeneration string, guard commitGuard) error {
	backend, ok := p.backend.(projectDeleter)
	if !ok {
		return errProjectDeleteUnavailable
	}
	p.deleteMu.Lock()
	defer p.deleteMu.Unlock()
	idle, err := p.store.beginProjectDeletionGuarded(actor, name, confirmation, expectedGeneration, guard)
	if err != nil {
		return err
	}
	select {
	case <-idle:
	case <-ctx.Done():
		return errProjectDeleteInterrupted
	}
	generation := p.store.ProjectGeneration(name)
	if fenced, ok := p.backend.(interface {
		DeleteProjectGeneration(context.Context, string, string) error
	}); ok {
		err = fenced.DeleteProjectGeneration(ctx, name, generation)
	} else {
		err = backend.DeleteProject(ctx, name)
	}
	if err == nil {
		err = p.store.finishProjectDeletion(name, generation)
	}
	if err != nil {
		return errProjectDeletePending
	}
	return nil
}
