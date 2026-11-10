package broker

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type RenameRequest = protocol.RenameRequest

// CheckSessionAccess keeps authorization and journal recovery under the same
// lock as rename, even for the runtime boundary's preliminary checks.
func (s *RemoteService) CheckSessionAccess(ctx context.Context, selector string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.find(selector)
	if e == nil {
		return ErrSessionNotFound
	}
	return s.authorizeLocked(ctx, e)
}

func bindingForRename(e *remoteSession) string {
	raw, _ := json.Marshal([3]string{e.record.Harness, e.record.SessionID, e.record.ClientID})
	return string(raw)
}

// RenameSession preserves the registration, active lease and queued envelopes.
// Both the lease and, on protected projects, the current credential must own it.
func (s *RemoteService) RenameSession(ctx context.Context, r RenameRequest) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	if !validDurableName(r.Name) || !registrationIDPattern.MatchString(r.RegistrationID) || !validLeaseID(r.ChannelID) {
		return Session{}, errors.New("invalid nickname or registration")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	e := s.byRegistration[r.RegistrationID]
	if e == nil {
		return Session{}, ErrSessionNotFound
	}
	if err := s.authorizeLocked(ctx, e); err != nil {
		return Session{}, err
	}
	if e.retired || e.record.Detached || e.record.Suspended || e.identity.ChannelID != r.ChannelID {
		return Session{}, errors.New("rename requires this session's active ingress lease")
	}
	if s.nameHolderBlocks(s.sessions[r.Name], e) {
		holder := s.sessions[r.Name]
		kind := "live-session"
		if holder.record != nil && holder.record.Detached {
			kind = "detached-registration"
		} else if s.snapshot(holder).Status != StatusAlive {
			kind = "inactive-registration"
		}
		rejection := protocol.RenameRejection{Code: "nickname-in-use", Holder: kind}
		if access.RequireScope(ctx, access.SessionRead) == nil {
			rejection.HolderRegistrationID = s.snapshot(holder).RegistrationID
		}
		return Session{}, rejection
	}
	if e.record.Name == r.Name {
		return s.snapshot(e), nil
	}
	return s.renameRecordLocked(ctx, e, r.Name)
}

func (s *RemoteService) renameRecordLocked(ctx context.Context, e *remoteSession, name string) (Session, error) {
	old := e.record.Name
	apply := func() error {
		next := *e.record
		next.Name, next.RequestedName = name, ""
		next.Revision++
		next.LastRequestID, next.LastRequestRevision = "", 0
		next.Probe = nil
		if err := s.persistLocked(&next); err != nil {
			return err
		}
		delete(s.sessions, old)
		s.sessions[name] = e
		*e.record = next
		e.identity.Name, e.session.Name = name, name
		s.notify(e)
		return nil
	}
	if grant, ok := access.FromContext(ctx); ok {
		if grant.Rename == nil {
			return Session{}, errors.New("credential does not support nickname changes")
		}
		if err := grant.Rename(bindingForRename(e), old, name, apply); err != nil {
			return Session{}, err
		}
	} else if err := apply(); err != nil {
		return Session{}, err
	}
	return s.snapshot(e), nil
}
