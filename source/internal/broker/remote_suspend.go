package broker

import (
	"context"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

var ErrSessionSuspended = errors.New("session is suspended")

func suspendedSession(e *remoteSession) bool {
	return e.record != nil && e.record.Suspended
}

// SetSuspended changes durable management policy without releasing the name
// or ownership binding. Only trusted management callers may invoke it; a
// session token grant never authorizes this operation, even for its own name.
// A successful transition discards pending messages and fences the receiver.
// Lifting suspension leaves the registration missing until a fresh lease is
// presented, with a new cleanup grace period.
func (s *RemoteService) SetSuspended(ctx context.Context, selector string, suspended bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, granted := access.FromContext(ctx); granted {
		return errors.New("session suspension requires trusted management access")
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
	return s.setSuspendedLocked(e, suspended, nil)
}
func (s *RemoteService) setSuspendedLocked(e *remoteSession, suspended bool, management *protocol.ManagementEvent) error {
	if e.record == nil {
		return errors.New("session suspension requires a durable registration")
	}
	if suspended && e.record.Detached {
		return errors.New("detached session must be explicitly registered before suspension")
	}
	if e.record.Suspended == suspended {
		return nil
	}
	now := s.now()
	next := *e.record
	next.Suspended = suspended
	if management != nil {
		next.LastManagement = management
	}
	if suspended && e.identity.ChannelID != "" {
		next.SuspendedLeaseID = e.identity.ChannelID
	}
	next.Revision++
	next.LastRequestID, next.LastRequestRevision = "", 0
	next.Probe = nil
	next.Detached, next.DetachedAt = false, time.Time{}
	if !suspended {
		next.LastSeen = now
	}
	if err := s.persistLocked(&next); err != nil {
		return errors.New("session suspension could not be recorded")
	}
	*e.record = next
	// Unlike retire, this also updates a receiver that was already missing.
	// Keep its current channel id fenced for the next explicit registration.
	e.retired = true
	e.clearActivity("lease-inactive")
	e.until = time.Time{}
	e.discardMessages()
	e.session.AutomaticDeliveryReady = false
	e.session.Status, e.session.ErrorCode = StatusMissing, ""
	if suspended {
		e.session.Status, e.session.ErrorCode = StatusSuspended, CodeSuspended
	}
	s.notify(e)
	return nil
}
