package broker

import (
	"context"
	"crypto/rand"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"time"
)

func sameReceiverHolder(a, b *protocol.ReceiverHolder) bool { return a != nil && b != nil && *a == *b }
func (s *RemoteService) holderBlockedLocked(e *remoteSession, h *protocol.ReceiverHolder, p *protocol.HolderProof, lease string) bool {
	if e.retired || !s.now().Before(e.until) {
		return false
	}
	if sameReceiverHolder(e.holder, h) {
		return false
	}
	// Round-4 compatibility amendment: legacy clients reconnect using a fresh
	// lease. Preserve that behavior only when BOTH holders lack metadata.
	// Mixed cases remain guarded; malformed non-nil holders are rejected by
	// request validation before reaching this function.
	if e.holder == nil && h == nil {
		return false
	}
	if e.holder != nil && h != nil && e.holder.DeviceID == h.DeviceID && p != nil && p.Dead && e.holderChallenge != nil && s.now().Before(e.holderChallengeUntil) {
		c := p.Challenge
		if c == *e.holderChallenge && c.Holder == *e.holder && c.Revision == e.record.Revision && c.Generation == e.record.LeaseGeneration {
			return false
		}
	}
	return true
}
func (s *RemoteService) holderChallengeLocked(e *remoteSession) *protocol.HolderChallenge {
	if e.holder == nil {
		return nil
	}
	c := e.holderChallenge
	if c == nil || c.Holder != *e.holder || c.Revision != e.record.Revision || c.Generation != e.record.LeaseGeneration || !s.now().Before(e.holderChallengeUntil) {
		c = &protocol.HolderChallenge{Holder: *e.holder, Revision: e.record.Revision, Generation: e.record.LeaseGeneration, Nonce: rand.Text()}
		e.holderChallenge = c
		e.holderChallengeUntil = s.now().Add(30 * time.Second)
	}
	copy := *c
	return &copy
}
func (s *RemoteService) holderResultLocked(e *remoteSession) RegisterResult {
	return RegisterResult{Code: protocol.CodeRegistrationInUse, HolderChallenge: s.holderChallengeLocked(e)}
}
func assignReceiverHolder(e *remoteSession, h *protocol.ReceiverHolder) {
	e.holder = nil
	if h != nil {
		copy := *h
		e.holder = &copy
	}
	e.holderChallenge = nil
}

// CheckRegistrationHolder performs a read-only preflight after the panel has
// authenticated the project-token owner. Rebind/Register/Resume repeat the
// same guard at mutation time; this avoids issuing credentials while waiting.
func (s *RemoteService) CheckRegistrationHolder(ctx context.Context, r RegisterRequest, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	e := s.byRegistration[id]
	g, ok := access.FromContext(ctx)
	if !ok || g.OwnerID == "" || e == nil || g.OwnerID != e.record.OwnerID || !secretMatches(e.record.SecretHash, r.Secret) || r.Harness != e.record.Harness || r.ClientID != e.record.ClientID || !protocol.ValidReceiverHolder(r.Holder) {
		return handoverRefused("owner-mismatch")
	}
	if s.holderBlockedLocked(e, r.Holder, r.HolderProof, r.ChannelID) {
		return protocol.HandoverRefusal{Reason: protocol.CodeRegistrationInUse, HolderChallenge: s.holderChallengeLocked(e)}
	}
	return nil
}
