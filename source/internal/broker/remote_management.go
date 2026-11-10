package broker

import (
	"context"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// ManagementCheck is server-only authorization. It runs under the mutation
// lock, must recheck live policy, and must not call back into this broker.
type ManagementCheck func(target Session, recordedOwner string, callers []Session) (protocol.ManagementEvent, error)

func (s *RemoteService) managementLocked(selector string, check ManagementCheck) (*remoteSession, protocol.ManagementEvent, error) {
	e := s.find(selector)
	if e == nil {
		return nil, protocol.ManagementEvent{}, ErrSessionNotFound
	}
	if check == nil {
		return nil, protocol.ManagementEvent{}, ErrSessionIdentityUnauthorized
	}
	var callers []Session
	for _, candidate := range s.sessions {
		v := s.snapshot(candidate)
		if candidate.record != nil && candidate.record.Detached {
			v.ErrorCode = CodeDetached
		}
		callers = append(callers, v)
	}
	owner := ""
	if e.record != nil {
		owner = e.record.OwnerID
	}
	event, err := check(s.snapshot(e), owner, callers)
	return e, event, err
}

func (s *RemoteService) CheckManagementAccess(ctx context.Context, selector string, check ManagementCheck) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _, err := s.managementLocked(selector, check)
	return err
}

// ManageSession is a trusted runtime path. Authorization is mandatory and is
// repeated inside the same lock as the durable write. SetSuspended's blanket
// rejection of session-granted callers is intentionally unchanged.
func (s *RemoteService) ManageSession(ctx context.Context, selector, kind string, check ManagementCheck, audit func(protocol.ManagementEvent) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, event, err := s.managementLocked(selector, check)
	if err != nil {
		return err
	}
	if e.record == nil {
		return errors.New("management requires a durable registration")
	}
	event.Kind = kind
	event.TargetName = e.record.Name
	event.TargetRegistrationID = e.record.RegistrationID
	event.At = s.now().UTC()
	// Self detach keeps the historical behavior and does not emit an other-session event.
	other := event.ActorRegistrationID != event.TargetRegistrationID
	var recorded *protocol.ManagementEvent
	if other {
		recorded = &event
	}
	switch kind {
	case "detached":
		err = s.stopSessionLocked(e, recorded)
	case "suspended":
		err = s.setSuspendedLocked(e, true, recorded)
	case "unsuspended":
		err = s.setSuspendedLocked(e, false, recorded)
	default:
		return errors.New("invalid management action")
	}
	if err != nil {
		return err
	}
	if other && audit != nil {
		return audit(event)
	}
	return nil
}
