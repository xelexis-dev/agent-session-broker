package broker

import (
	"context"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// RetireManaged keeps authorization, detach, policy cleanup and removal in one
// mutation lock. cleanup must not call the broker; lock order is broker -> store.
func (s *RemoteService) RetireManaged(ctx context.Context, r protocol.RetireRequest, check ManagementCheck, cleanup func(access.RetirementTarget, func() error) ([]protocol.RevokedCredential, error)) (protocol.RetireResult, error) {
	fail := func(code string) (protocol.RetireResult, error) {
		return protocol.RetireResult{}, protocol.RejectRetire(code)
	}
	if err := ctx.Err(); err != nil {
		return protocol.RetireResult{}, err
	}
	if (r.Name == "") == (r.RegistrationID == "") || (r.Name != "" && !validDurableName(r.Name)) || (r.RegistrationID != "" && !registrationIDPattern.MatchString(r.RegistrationID)) {
		return fail("invalid-selector")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cleanup == nil || check == nil {
		return fail("forbidden")
	}
	var callers []Session
	for _, e := range s.sessions {
		callers = append(callers, s.snapshot(e))
	}
	if r.Name != "" {
		// Validate the caller before revealing holders or selecting orphan grants.
		if _, err := check(Session{}, "", callers); err != nil {
			return protocol.RetireResult{}, err
		}
		if holder := s.sessions[r.Name]; holder != nil {
			rejection := protocol.RejectRetire("name-held-by-registration")
			if access.RequireScope(ctx, access.SessionRead) == nil {
				rejection.HolderRegistrationID = s.snapshot(holder).RegistrationID
			}
			return protocol.RetireResult{}, rejection
		}
		revoked, err := cleanup(access.RetirementTarget{Name: r.Name}, nil)
		if err != nil {
			return protocol.RetireResult{}, err
		}
		return protocol.RetireResult{OK: true, Mode: "credential", Name: r.Name, RevokedCredentials: revoked}, nil
	}
	e := s.byRegistration[r.RegistrationID]
	if e == nil {
		return fail("not-found")
	}
	event, err := check(s.snapshot(e), e.record.OwnerID, callers)
	if err != nil {
		return protocol.RetireResult{}, err
	}
	if e.record.Suspended {
		return fail("target-suspended")
	}
	target := access.RetirementTarget{Identity: bindingForRename(e), Name: e.record.Name, RecordedOwner: e.record.OwnerID, RegistrationID: r.RegistrationID}
	event.Kind, event.Retired, event.TargetName, event.TargetRegistrationID, event.At = "detached", true, e.record.Name, r.RegistrationID, s.now().UTC()
	target.Event = event
	discarded := len(e.queue) + len(e.inflight)
	revoked, err := cleanup(target, func() error { return s.stopSessionLocked(e, nil) })
	var refusal protocol.RetireRejection
	if err != nil && (!e.record.Detached || (errors.As(err, &refusal) && refusal.Code != "storage")) {
		return protocol.RetireResult{}, err
	}
	if err == nil {
		err = s.removeLocked(e, CodeDetached)
	}
	if err != nil {
		rejection := protocol.RejectRetire("cleanup-required")
		rejection.RegistrationID = r.RegistrationID
		return protocol.RetireResult{}, rejection
	}
	return protocol.RetireResult{OK: true, Mode: "registration", Name: target.Name, RegistrationID: r.RegistrationID, RevokedCredentials: revoked, DiscardedMessages: discarded}, nil
}
