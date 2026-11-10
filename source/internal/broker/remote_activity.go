package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

const activityTTL = 25 * time.Second
const activityFutureTolerance = time.Minute
const activityMaxBytes = 2048

func (e *remoteSession) clearActivity(reason string) {
	e.activity = &protocol.ActivitySnapshot{Activity: protocol.Activity{State: protocol.ActivityUnknown, Reason: reason}}
	e.activityAuthorization = nil
}

// activitySnapshot runs under the service lock. Returning a copy prevents a
// caller from modifying either stored evidence or its expiry.
func (s *RemoteService) activitySnapshot(e *remoteSession) *protocol.ActivitySnapshot {
	if e.retired || !e.session.AutomaticDeliveryReady {
		e.clearActivity("lease-inactive")
	} else if e.activityAuthorization != nil && e.activityAuthorization() != nil {
		e.clearActivity("lease-inactive")
	}
	if e.activity == nil {
		e.clearActivity("evidence-missing")
	}
	v := *e.activity
	if !v.ExpiresAt.IsZero() && !s.now().Before(v.ExpiresAt) {
		v.State, v.Reason = protocol.ActivityUnknown, "observation-expired"
	}
	return &v
}

func validActivityReason(reason string) bool {
	switch reason {
	case "", "unsupported", "evidence-missing", "identity-mismatch", "malformed-evidence", "stale-evidence", "budget-exceeded", "lease-inactive", "observer-unavailable", "observation-expired":
		return true
	}
	return false
}

func utcActivityTime(value time.Time) bool {
	_, offset := value.Zone()
	return !value.IsZero() && offset == 0
}

// applyActivityLocked is called only after authorization, registration policy,
// and exact current lease checks. Invalid activity cannot reject the heartbeat.
func (s *RemoteService) applyActivityLocked(ctx context.Context, e *remoteSession, raw json.RawMessage, now time.Time) {
	if !e.session.AutomaticDeliveryReady {
		e.clearActivity("lease-inactive")
		return
	}
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		e.clearActivity("evidence-missing")
		return
	}
	var sample protocol.Activity
	if len(raw) > activityMaxBytes {
		e.clearActivity("malformed-evidence")
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sample); err != nil {
		e.clearActivity("malformed-evidence")
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		e.clearActivity("malformed-evidence")
		return
	}
	// Generation is only an additional consistency check, never authority.
	if sample.Generation != 0 && sample.Generation != e.session.Generation {
		return
	}
	if sample.Sequence == 0 {
		e.clearActivity("malformed-evidence")
		return
	}
	if sample.Sequence <= e.activitySequence {
		return
	}
	validState := sample.State == protocol.ActivityWorking || sample.State == protocol.ActivityIdle || sample.State == protocol.ActivityUnknown
	validSource := sample.Source == "claude-session-file" || sample.Source == "codex-rollout" || (sample.State == protocol.ActivityUnknown && sample.Source == "")
	if !validState || !validSource || !validActivityReason(sample.Reason) || !utcActivityTime(sample.ObservedAt) ||
		sample.ObservedAt.After(now.Add(activityFutureTolerance)) ||
		(!sample.ChangedAt.IsZero() && (!utcActivityTime(sample.ChangedAt) || sample.ChangedAt.After(sample.ObservedAt))) {
		e.clearActivity("malformed-evidence")
		return
	}
	e.activitySequence = sample.Sequence
	if sample.ObservedAt.Before(now.Add(-activityTTL)) || sample.ObservedAt.Before(e.activityObservedAt) {
		e.clearActivity("stale-evidence")
		return
	}
	e.activityObservedAt = sample.ObservedAt
	sample.Generation = e.session.Generation
	e.activity = &protocol.ActivitySnapshot{Activity: sample, ReceivedAt: now.UTC(), ExpiresAt: now.Add(activityTTL).UTC()}
	// Retain only the server-side revocation check and bound identity, never a
	// bearer token or the entire grant. Queries can then immediately invalidate
	// accepted evidence even when a revoked request never reaches RenewLease.
	e.activityAuthorization = nil
	if grant, ok := access.FromContext(ctx); ok && grant.Check != nil {
		check, identity := grant.Check, bindingForRename(e)
		e.activityAuthorization = func() error { return check(identity) }
	}
}

// SessionActivityOutputAllowed is a compatibility hint for strict MCP output
// schemas, not an authorization decision. Only the authenticated caller's own
// current lease can prove support. Freshness of display evidence is separate:
// once a valid sample was accepted, TTL expiry or a missing sample does not
// downgrade that lease. issueLeaseLocked resets this proof on replacement.
func (s *RemoteService) SessionActivityOutputAllowed(ctx context.Context) bool {
	grant, ok := access.FromContext(ctx)
	if !ok || grant.Check == nil || ctx.Err() != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.byRegistration {
		if e.retired || e.record.Detached || e.record.Suspended || !s.now().Before(e.until) || e.activityObservedAt.IsZero() {
			continue
		}
		if grant.Check(bindingForRename(e)) == nil {
			return true
		}
	}
	return false
}
