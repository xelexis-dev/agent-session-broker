package broker

import (
	"context"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

// BugReportSession selects a server-owned registration by its credential binding,
// without requiring roster scopes or accepting a caller-supplied session selector.
func (s *RemoteService) BugReportSession(ctx context.Context) (Session, error) {
	return s.bugReportSession(ctx, false)
}

// BugReportManagementSession additionally requires a live, non-suspended lease.
func (s *RemoteService) BugReportManagementSession(ctx context.Context) (Session, error) {
	return s.bugReportSession(ctx, true)
}
func (s *RemoteService) bugReportSession(ctx context.Context, live bool) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	grant, ok := access.FromContext(ctx)
	if !ok || grant.TokenID == "" || grant.Project == "" || grant.OwnerID == "" || grant.Check == nil || (!grant.ExpiresAt.IsZero() && !time.Now().Before(grant.ExpiresAt)) {
		return Session{}, ErrSessionIdentityUnauthorized
	}
	select {
	case <-grant.Revoked:
		return Session{}, ErrSessionIdentityUnauthorized
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	for _, e := range s.sessions {
		if e.record == nil || e.record.RegistrationID == "" || e.record.Detached {
			continue
		}
		if live && (e.retired || e.record.Suspended || !s.now().Before(e.until)) {
			continue
		}
		if grant.Check(bindingForRename(e)) != nil {
			continue
		}
		if err := s.authorizeLocked(ctx, e); err != nil {
			return Session{}, err
		}
		return s.snapshot(e), nil
	}
	return Session{}, ErrSessionIdentityUnauthorized
}
