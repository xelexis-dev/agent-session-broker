package channel

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func followSettleFixture(t *testing.T) (*claudeFollow, *durableOptions, *ingressBinding, broker.AttachRequest) {
	t.Helper()
	f, opts, id, _ := fastFollowFixture(t)
	b := f.lookup(id.SessionID)
	b.identity.ChannelID = "synthetic-lease"
	b.generation = 7
	b.ingressReady, b.ingressReadyChannel, b.ingressReadyGeneration = true, b.identity.ChannelID, b.generation
	b.done = make(chan struct{})
	b.sink = func(context.Context, broker.Message) error { return nil }
	opts.claudeSessionFile = func(int) ([]byte, error) {
		return json.Marshal(map[string]any{"pid": id.PID, "cwd": id.CWD, "kind": "interactive", "sessionId": id.SessionID})
	}
	f.skipped(b, "unavailable")
	return f, opts, b, id
}

func TestFollowSettleFullObserveEvenWithUnchangedSessionFile(t *testing.T) {
	f, opts, b, id := followSettleFixture(t)
	observations, keys := 0, 0
	opts.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) {
		observations++
		return id, nil
	}
	opts.parentProcessKey = func(context.Context, int) (string, error) { keys++; return "same", nil }
	if _, err := f.check(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if b.receiveReason != "" || b.receiveGate != "" || observations != 1 || keys != 2 {
		t.Fatalf("full settle proof missing: observes=%d keys=%d blocked=%t", observations, keys, b.receiveReason != "")
	}
}

func TestFollowSettleKeepsEveryIndependentGate(t *testing.T) {
	for _, name := range []string{"not-started", "failed", "retired", "not-ready", "old-ready-channel", "old-ready-generation", "missing-receiver", "receiver-ended", "sink-unavailable", "follow-stopped", "no-sink", "resume-stop", "suspended", "holder-wait", "activation-pending", "receive-denied", "project-error", "detached", "other-owner", "other-reason", "parent-changed", "detached-follow", "suspended-follow"} {
		t.Run(name, func(t *testing.T) {
			f, opts, b, _ := followSettleFixture(t)
			switch name {
			case "not-started":
				b.started = false
			case "failed":
				b.failed = true
			case "retired":
				b.retired = true
			case "not-ready":
				b.ingressReady = false
			case "old-ready-channel":
				b.ingressReadyChannel = "old-lease"
			case "old-ready-generation":
				b.ingressReadyGeneration--
			case "missing-receiver":
				b.done = nil
			case "receiver-ended":
				close(b.done)
			case "sink-unavailable":
				b.receiveUnavailable = true
			case "follow-stopped":
				b.followStopped = true
			case "no-sink":
				b.sink = nil
			case "resume-stop":
				b.resumeStopReason = "registration-detached"
			case "suspended":
				b.resumeSuspended = true
			case "holder-wait":
				b.holderWaiting = true
			case "activation-pending":
				opts.ready = make(chan struct{})
			case "receive-denied":
				defaults := (durableOptions{}).withDefaults()
				b.upstream = defaults.wrapReceive(&unavailableUpstream{}, "synthetic").(*receiveUpstream)
				b.upstream.(*receiveUpstream).currentPermission().decide(receiveForbidden)
			case "project-error":
				u := &brokerUpstream{}
				u.recordProjectTerminal(&projectRefusalError{code: "project_deleted", terminal: true})
				b.upstream = u
			case "detached":
				b.durable.detached = true
			case "other-owner":
				b.receiveGate = ""
			case "other-reason":
				b.receiveReason = "project-deleted"
			case "parent-changed", "detached-follow", "suspended-follow":
				reason := name
				if name == "detached-follow" {
					reason = "detached"
				}
				if name == "suspended-follow" {
					reason = "suspended"
				}
				f.skipped(b, reason)
			}
			reason, gate := b.receiveReason, b.receiveGate
			f.mu.Lock()
			f.settle(t.Context(), b)
			f.mu.Unlock()
			if b.receiveReason != reason || b.receiveGate != gate {
				t.Fatal("settle cleared an independent or unverified gate")
			}
		})
	}
}

func TestFollowSettleRejectsChangesDuringFullObservation(t *testing.T) {
	for _, name := range []string{"key-before", "key-after", "observe-error", "pid", "cwd", "conversation", "generation", "channel", "receiver-end", "project-gate", "late-old-binding"} {
		t.Run(name, func(t *testing.T) {
			f, opts, b, id := followSettleFixture(t)
			keys, observations := 0, 0
			opts.parentProcessKey = func(context.Context, int) (string, error) {
				keys++
				if name == "key-before" || (name == "key-after" && keys > 1) {
					return "reused-process", nil
				}
				return "same", nil
			}
			opts.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) {
				observations++
				b.mu.Lock()
				defer b.mu.Unlock()
				switch name {
				case "observe-error":
					return broker.AttachRequest{}, errors.New("synthetic unavailable")
				case "pid":
					id.PID++
				case "cwd":
					id.CWD = "/other-synthetic-cwd"
				case "conversation":
					id.SessionID = claudeHandoverIdentity(999, id.PID).SessionID
				case "generation":
					b.generation++
					b.ingressReadyGeneration = b.generation
				case "channel":
					b.identity.ChannelID = "new-lease"
					b.ingressReadyChannel = b.identity.ChannelID
				case "receiver-end":
					close(b.done)
				case "project-gate":
					b.receiveReason, b.receiveGate = "project-deleted", ""
				case "late-old-binding":
					f.lookup = func(string) *ingressBinding { return &ingressBinding{} }
				}
				return id, nil
			}
			f.mu.Lock()
			f.settle(t.Context(), b)
			f.mu.Unlock()
			if b.receiveReason == "" || (name == "project-gate" && b.receiveReason != "project-deleted") {
				t.Fatal("settle cleared a stale observation or newer cause")
			}
			if name == "key-before" && observations != 0 {
				t.Fatal("observed after parent replacement")
			}
		})
	}
}

func TestFollowSkipCannotClaimAnotherFailureGate(t *testing.T) {
	f, _, b, _ := followSettleFixture(t)
	f.deliveryFailed(b, errors.New("synthetic sink failure"))
	f.skipped(b, "unavailable")
	f.mu.Lock()
	f.settle(t.Context(), b)
	f.mu.Unlock()
	if b.receiveGate != "" || b.receiveReason != "reconnect-required" || !b.receiveUnavailable {
		t.Fatal("follow claimed or cleared the sink failure gate")
	}
}
