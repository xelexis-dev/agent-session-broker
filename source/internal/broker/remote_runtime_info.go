package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"io"
	"time"
)

// Called after authorization and exact current lease checks. Invalid display
// metadata never fails a heartbeat and cannot mutate registration status.
func (s *RemoteService) applyRuntimeInfoLocked(ctx context.Context, e *remoteSession, raw json.RawMessage, now time.Time) {
	clear := func() { e.runtimeInfo = nil; e.runtimeAuthorization = nil }
	if len(raw) == 0 {
		clear()
		return
	}
	var v protocol.RuntimeInfo
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if len(raw) > 1024 || dec.Decode(&v) != nil || dec.Decode(new(any)) != io.EOF {
		clear()
		return
	}
	if v.Generation != e.session.Generation {
		return
	}
	if v.Sequence == 0 || !protocol.ValidRuntimeLabel(v.Model, 96) || !protocol.ValidRuntimeLabel(v.Effort, 16) || !utcActivityTime(v.ObservedAt) || v.ObservedAt.After(now.Add(time.Minute)) {
		clear()
		return
	}
	if v.Sequence <= e.runtimeSequence {
		return
	}
	e.runtimeSequence = v.Sequence
	if v.ObservedAt.Before(now.Add(-activityTTL)) || v.ObservedAt.Before(e.runtimeObservedAt) {
		clear()
		return
	}
	e.runtimeObservedAt = v.ObservedAt
	e.runtimeInfo = &protocol.RuntimeInfoSnapshot{RuntimeInfo: v, ReceivedAt: now.UTC(), ExpiresAt: now.Add(activityTTL).UTC()}
	e.runtimeAuthorization = nil
	if grant, ok := access.FromContext(ctx); ok && grant.Check != nil {
		check, identity := grant.Check, bindingForRename(e)
		e.runtimeAuthorization = func() error { return check(identity) }
	}
}

// SessionRuntimeInfo is an internal panel projection, never part of MCP Session.
// The panel performs its ordinary project membership check before calling it.
func (s *RemoteService) SessionRuntimeInfo() map[string]protocol.RuntimeInfoSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]protocol.RuntimeInfoSnapshot)
	for id, e := range s.byRegistration {
		if e.runtimeInfo == nil {
			continue
		}
		if e.retired || e.record.Detached || e.record.Suspended || !s.now().Before(e.until) || (e.runtimeAuthorization != nil && e.runtimeAuthorization() != nil) {
			e.runtimeInfo = nil
			e.runtimeAuthorization = nil
			continue
		}
		if !s.now().Before(e.runtimeInfo.ExpiresAt) {
			continue
		}
		out[id] = *e.runtimeInfo
	}
	return out
}
