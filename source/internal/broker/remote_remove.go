package broker

import (
	"context"
	"errors"
)

// RemoveDetachedSession releases a detached registration. The
// admission cleanup runs under the broker lock, before removal, so an explicit
// re-registration cannot race token revocation. cleanup must not call the broker.
// StopSession itself retains its MCP contract of allowing explicit re-registration.
// For legacy (non-durable) entries, callers must fence replaying legacy clients
// before removal; the managed production runtime accepts token-based clients only.
func (s *RemoteService) RemoveDetachedSession(ctx context.Context, selector string, cleanup func(Session) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.find(selector)
	if e == nil {
		return ErrSessionNotFound
	}
	if err := s.authorizeLocked(ctx, e); err != nil {
		return err
	}
	if !e.retired || e.session.ErrorCode != CodeDetached || (e.record != nil && !e.record.Detached) {
		return errors.New("only detached sessions can be removed")
	}
	if cleanup != nil {
		if err := cleanup(s.snapshot(e)); err != nil {
			return err
		}
	}
	if e.record != nil {
		return s.removeLocked(e, CodeDetached)
	}
	delete(s.sessions, e.session.Name)
	return nil
}

// RemoveDetachedSessions skips invalid or unauthorized rows independently. Policy
// commits before one storage transaction; failures retain every accepted row.
func (s *RemoteService) RemoveDetachedSessions(ctx context.Context, selectors []string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var entries []*remoteSession
	var removed []string
	seen := map[*remoteSession]bool{}
	for _, selector := range selectors {
		e := s.find(selector)
		if e == nil || seen[e] || s.authorizeLocked(ctx, e) != nil {
			continue
		}
		if !e.retired || e.session.ErrorCode != CodeDetached || (e.record != nil && !e.record.Detached) {
			continue
		}
		seen[e] = true
		entries = append(entries, e)
		removed = append(removed, selector)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.removeEntriesLocked(entries, CodeDetached); err != nil {
		return nil, err
	}
	return removed, nil
}
