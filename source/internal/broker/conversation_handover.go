package broker

import (
	"context"
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"slices"
	"time"
)

func handoverRefused(reason string) error { return protocol.HandoverRefusal{Reason: reason} }
func (s *RemoteService) handoverProofLocked(r RegisterRequest, id string) (*remoteSession, error) {
	e := s.byRegistration[id]
	if e == nil {
		return nil, handoverRefused("not-found")
	}
	if !secretMatches(e.record.SecretHash, r.Secret) {
		return nil, handoverRefused("secret-mismatch")
	}
	if r.Harness != "claude" || e.record.Harness != r.Harness || e.record.ClientID != r.ClientID {
		return nil, handoverRefused("client-mismatch")
	}
	if e.record.SessionID == r.SessionID && e.record.LastRequestID != r.RequestID {
		return nil, handoverRefused("identity-conflict")
	}
	if e.record.Suspended {
		return nil, handoverRefused("suspended")
	}
	if other := s.durableIdentityLocked(r.Harness, r.SessionID, r.ClientID); other != nil && other != e {
		return nil, handoverRefused("identity-conflict")
	}
	if !protocol.ValidReceiverHolder(r.Holder) || r.PID <= 0 || len(r.ProcessKey) > 512 || !validCanonicalConversationID(r.SessionID) || !validRemoteCWD(r.CWD) {
		return nil, handoverRefused("identity-conflict")
	}
	if e.record.SessionID != r.SessionID && s.snapshot(e).Status == StatusAlive && e.record.PID != r.PID {
		return nil, handoverRefused("still-receiving")
	}
	return e, nil
}
func (s *RemoteService) InspectConversationHandover(ctx context.Context, r RegisterRequest, id string) (DirectProof, error) {
	if err := ctx.Err(); err != nil {
		return DirectProof{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	e, err := s.handoverProofLocked(r, id)
	if err != nil {
		return DirectProof{}, err
	}
	raw, _ := json.Marshal([3]string{e.record.Harness, e.record.SessionID, e.record.ClientID})
	return DirectProof{HandoverFrom: e.record.HandoverFrom, RegistrationID: id, OwnerID: e.record.OwnerID, Name: e.record.Name, Revision: e.record.Revision, AdmissionGrantID: e.record.AdmissionGrantID, PreviousIdentity: string(raw)}, nil
}

// Broker -> policy is the only nested lock order. The policy callback rechecks
// the issued child/owner while the broker's proof and receiving state are locked.
func (s *RemoteService) RebindConversation(ctx context.Context, r RegisterRequest, id string) (DirectProof, error) {
	if err := access.RequireScope(ctx, access.SessionRegister); err != nil {
		return DirectProof{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	e, err := s.handoverProofLocked(r, id)
	if err != nil {
		return DirectProof{}, err
	}
	g, ok := access.FromContext(ctx)
	if !ok || g.OwnerID == "" || g.OwnerID != e.record.OwnerID {
		return DirectProof{}, handoverRefused("owner-mismatch")
	}
	if s.holderBlockedLocked(e, r.Holder, r.HolderProof, "") {
		return DirectProof{}, protocol.HandoverRefusal{Reason: protocol.CodeRegistrationInUse, HolderChallenge: s.holderChallengeLocked(e)}
	}
	if e.record.SessionID == r.SessionID && e.record.LastRequestID == r.RequestID {
		if err := s.updateReattachCapabilityLocked(e, r.Capabilities); err != nil {
			return DirectProof{}, err
		}
		return DirectProof{HandoverFrom: e.record.HandoverFrom, RegistrationID: id, OwnerID: e.record.OwnerID, Name: e.record.Name, Revision: e.record.Revision, AdmissionGrantID: e.record.AdmissionGrantID}, nil
	}
	if e.record.Revision != r.ExpectedRevision {
		return DirectProof{}, handoverRefused("revision-mismatch")
	}
	nextSession := s.snapshot(e)
	nextSession.ID = r.SessionID
	nextSession.PID = r.PID
	if err := s.authorizeSessionLocked(ctx, nextSession); err != nil {
		return DirectProof{}, err
	}
	if err := s.directFenceLocked(ctx, r.Harness, r.SessionID, r.ClientID, id, e.record.Name, e); err != nil {
		return DirectProof{}, err
	}
	next := *e.record
	now := s.now()
	next.HandoverFrom = &protocol.HandoverFrom{Revision: next.Revision, At: now}
	next.SessionID, next.PID, next.CWD = r.SessionID, r.PID, r.CWD
	next.ProcessKey = r.ProcessKey
	next.Revision++
	next.Detached = false
	next.DetachedAt = time.Time{}
	next.LastSeen = now
	next.LastRequestID = r.RequestID
	next.LastRequestRevision = r.ExpectedRevision
	next.Probe = nil
	s.admissionMetadata(ctx, &next)
	next.ReattachAfterCleanup = slices.Contains(r.Capabilities, protocol.ReattachAfterCleanupCapability)
	if err := s.persistLocked(&next); err != nil {
		return DirectProof{}, err
	}
	if e.identity.ChannelID != "" {
		e.leases[e.identity.ChannelID] = struct{}{}
	}
	*e.record = next
	e.protectAckLoss(now)
	e.requeueInflight()
	if len(e.queue) != 0 && e.queueHeldUntil.IsZero() {
		e.queueHeldUntil = now.Add(expiredIngressQueueRetention)
	}
	assignReceiverHolder(e, r.Holder)
	e.identity.SessionID, e.identity.PID, e.identity.CWD = r.SessionID, r.PID, r.CWD
	e.identity.ChannelID = ""
	e.session.ID = r.SessionID
	e.session.PID = r.PID
	e.retired = true
	e.session.Status = StatusMissing
	e.session.ErrorCode = "handover-pending"
	e.session.AutomaticDeliveryReady = false
	// Do not call retire: queued envelopes belong to the registration, and must
	// survive changing its conversation. Closing changed fences the old lease.
	s.notify(e)
	return DirectProof{HandoverFrom: e.record.HandoverFrom, RegistrationID: id, OwnerID: next.OwnerID, Name: next.Name, Revision: next.Revision, AdmissionGrantID: next.AdmissionGrantID}, nil
}
