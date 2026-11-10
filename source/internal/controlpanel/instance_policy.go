package controlpanel

import "maps"

// AllowsUnauthenticated remains for callers compiled against the old store API.
// Project access always requires a session token.
func (s *Store) AllowsUnauthenticated() bool { return false }

// SetAllowsUnauthenticated cannot re-enable the removed authentication mode.
func (s *Store) SetAllowsUnauthenticated(actor string, allowed bool) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.data.Admin == nil {
		return ErrStorage
	}
	if !s.isAdminLocked(actor) || allowed {
		return ErrForbidden
	}
	return nil
}

// migrateTokenOnly persists the access restriction before accepting requests.
// Keep legacy fields readable, and retain every project field except its mode.
func (s *Store) migrateTokenOnly() error {
	candidate := s.data
	candidate.Projects = maps.Clone(s.data.Projects)
	changed := !candidate.DisableUnauthenticated
	candidate.DisableUnauthenticated = true
	for name, p := range candidate.Projects {
		if p.Mode == "open" {
			p.Mode = "token"
			candidate.Projects[name] = p
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.commit(candidate)
}

// migrateHTTPPolicy folds the removed per-project HTTP opt-in into the
// instance policy exactly once. It runs only while a project still carries the
// old flag, so it never rewrites already migrated state. Mixed settings fail
// closed: HTTP stays off until an administrator enables it for the instance.
func (s *Store) migrateHTTPPolicy() error {
	pending, found, all := false, false, true
	for _, p := range s.data.Projects {
		pending = pending || p.AllowHTTP
		if p.Deleting {
			continue
		}
		found = true
		all = all && p.AllowHTTP
	}
	if !pending {
		return nil
	}
	candidate := s.data
	candidate.AllowHTTP = found && all
	candidate.Projects = maps.Clone(s.data.Projects)
	for name, p := range candidate.Projects {
		p.AllowHTTP = false
		candidate.Projects[name] = p
	}
	return s.commit(candidate)
}
