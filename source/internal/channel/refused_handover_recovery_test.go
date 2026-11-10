package channel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestRefusedHandoverPollRestoresOriginalNativeInbox(t *testing.T) {
	r := newDetachedRuntime(t)
	initial := claudeHandoverIdentity(801, 10101)
	next := claudeHandoverIdentity(802, initial.PID)
	roster := &followRoster{id: initial}
	clock := &followClock{now: time.Unix(1000, 0), ticks: make(chan followTick, 1)}
	native := followNativeInbox(t, initial)
	var b *ingressBinding
	h := r.identityHarness(t, t.TempDir(), initial, func(o *durableOptions) {
		followOptions(roster)(o)
		o.claudeFollowClock = clock.clock()
		o.device.authorizer = &followLegacyAuthorizer{o.device.authorizer.(*deviceAuthClient)}
		o.claudeInboxFactory = newClaudeInboxFactory(t.Context(), initial, roster.observe, o.parentProcessKey)
		o.bindingCreated = func(binding *ingressBinding) {
			if binding.identity.SessionID == initial.SessionID {
				b = binding
			}
		}
	})
	h.attach(t)
	manager := r.harness(t, t.TempDir(), 10102)
	manager.attach(t)
	roster.set(next)
	if err := call(t.Context(), manager.client, "send_message", broker.Message{From: manager.session.Name, To: h.session.Name, Body: "refused-pending"}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "refused handover pauses original receiver", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.failed && b.receiveUnavailable && b.followMessage != nil
	})
	select {
	case <-native:
		t.Fatal("message delivered while current conversation was C1")
	default:
	}
	roster.set(initial)
	select {
	case tick := <-clock.ticks:
		close(tick.release)
	case <-time.After(time.Second):
		t.Fatal("periodic check not scheduled")
	}
	select {
	case m := <-native:
		if m.Body != "refused-pending" {
			t.Fatal("wrong pending message")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("periodic check did not restore C0 native inbox")
	}
	waitFor(t, "receiver restored", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return !b.failed && !b.receiveUnavailable && b.receiveReason == "" && b.followMessage == nil
	})
	if err := call(t.Context(), manager.client, "send_message", broker.Message{From: manager.session.Name, To: h.session.Name, Body: "after-return"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-native:
		if m.Body != "after-return" {
			t.Fatal("duplicate pending message")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restored receiver did not receive next message")
	}
	select {
	case <-native:
		t.Fatal("duplicate native delivery")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRefusedHandoverCooldownAndConversationChanges(t *testing.T) {
	for _, mode := range []string{"move", "startup"} {
		for _, reason := range []string{"unsupported-mode", "suspended"} {
			t.Run(mode+"/"+reason, func(t *testing.T) {
				f, opts, initial, _ := fastFollowFixture(t)
				current := claudeHandoverIdentity(812, initial.PID)
				now := time.Unix(1000, 0)
				opts.claudeFollowClock = devicePollClock{now: func() time.Time { return now }, sleep: func(context.Context, time.Duration) error { return nil }}
				opts.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return current, nil }
				opts.claudeSessionFile = func(int) ([]byte, error) {
					return json.Marshal(map[string]any{"pid": current.PID, "cwd": current.CWD, "kind": "interactive", "sessionId": current.SessionID})
				}
				attempts := 0
				refuse := func() (broker.AttachRequest, error) {
					attempts++
					return current, broker.HandoverRefusal{Reason: reason}
				}
				f.move = func(context.Context, broker.AttachRequest, broker.AttachRequest) (broker.AttachRequest, error) {
					return refuse()
				}
				if mode == "startup" {
					f.lookup = func(string) *ingressBinding { return nil }
					f.noRegistered = func() bool { return true }
					f.startup = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return refuse() }
				}
				_, _ = f.check(t.Context(), initial)
				delay := 30 * time.Second
				if reason == "unsupported-mode" {
					delay = 10 * time.Minute
				}
				began := now
				for now = began.Add(2 * time.Second); now.Before(began.Add(delay)); now = now.Add(2 * time.Second) {
					_, _ = f.check(t.Context(), initial)
				}
				if attempts != 1 || !strings.Contains(f.summary(), "follow-skipped: "+reason) {
					t.Fatalf("refusal cooldown failed: attempts=%d", attempts)
				}
				_, _ = f.check(t.Context(), initial)
				if attempts != 2 {
					t.Fatal("due retry did not run")
				}
				current = claudeHandoverIdentity(813, initial.PID)
				_, _ = f.check(t.Context(), initial)
				if attempts != 3 {
					t.Fatal("new conversation did not bypass refusal cooldown")
				}
				if mode == "move" {
					current = initial
					_, _ = f.check(t.Context(), initial)
					current = claudeHandoverIdentity(813, initial.PID)
					_, _ = f.check(t.Context(), initial)
					if attempts != 4 {
						t.Fatal("return then switch did not re-evaluate immediately")
					}
				}
			})
		}
	}
}

func TestRefusedHandoverRestoreFailureDiagnosticAndRetry(t *testing.T) {
	f, opts, id, _ := fastFollowFixture(t)
	store, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts.state = store
	b := f.lookup(id.SessionID)
	b.identity = id
	b.durable.key = strings.Repeat("a", 32)
	b.followStopped = true
	b.failed = true
	b.receiveUnavailable = true
	b.receiveReason = "reconnect-required"
	b.followMessage = &broker.Message{From: "private-sender", Body: "private-pending-body"}
	now := time.Unix(1000, 0)
	opts.claudeFollowClock = devicePollClock{now: func() time.Time { return now }, sleep: func(context.Context, time.Duration) error { return nil }}
	opts.claudeSessionFile = func(int) ([]byte, error) {
		return json.Marshal(map[string]any{"pid": id.PID, "cwd": id.CWD, "kind": "interactive", "sessionId": id.SessionID})
	}
	opts.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) {
		t.Fatal("same-conversation recovery should run before fast return")
		return id, nil
	}
	attempts, delivered := 0, 0
	b.sink = func(context.Context, broker.Message) error { delivered++; return nil }
	f.restore = func(context.Context, *ingressBinding) error {
		attempts++
		if attempts == 1 {
			return errors.New("private-error-detail")
		}
		b.failed = false
		b.receiveUnavailable = false
		return nil
	}
	_, _ = f.check(t.Context(), id)
	summary := opts.diagnostics(b)
	if summary == nil || summary.LastFailureReason != "reconnect-failed" || !b.followStopped || b.followMessage == nil {
		t.Fatal("failed restore lost message or diagnostic")
	}
	for i := 0; i < 14; i++ {
		now = now.Add(2 * time.Second)
		_, _ = f.check(t.Context(), id)
	}
	if attempts != 1 || delivered != 0 {
		t.Fatal("restore retried too soon or delivered before resume")
	}
	now = now.Add(2 * time.Second)
	_, _ = f.check(t.Context(), id)
	_, _ = f.check(t.Context(), id)
	if attempts != 2 || delivered != 1 || b.followMessage != nil || b.followStopped || b.receiveReason != "" {
		t.Fatal("restore did not recover exactly once")
	}
	diagnostics, err := opts.state.LoadDiagnostics(b.durable.key)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(diagnostics)
	if strings.Contains(string(raw), "private-") {
		t.Fatal("diagnostics retained message, sender or error detail")
	}
}

func TestRefusedHandoverCloseRecordsPendingDrop(t *testing.T) {
	r := newDetachedRuntime(t)
	id := claudeHandoverIdentity(820, 10301)
	roster := &followRoster{id: id}
	var b *ingressBinding
	var opts durableOptions
	h := r.identityHarness(t, t.TempDir(), id, func(o *durableOptions) {
		followOptions(roster)(o)
		opts = *o
		o.bindingCreated = func(binding *ingressBinding) { b = binding }
	})
	h.attach(t)
	b.mu.Lock()
	b.followMessage = &broker.Message{From: "private-sender", Body: "private-pending-body"}
	b.mu.Unlock()
	h.close()
	diagnostics, err := opts.state.LoadDiagnostics(b.durable.key)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range diagnostics.Events {
		if event.Event == "follow-message-dropped" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("pending close diagnostic count=%d", count)
	}
	raw, _ := json.Marshal(diagnostics)
	if strings.Contains(string(raw), "private-") {
		t.Fatal("close diagnostic retained message or sender")
	}
}

type toggledHandoverAuthorizer struct {
	*deviceAuthClient
	allow *atomic.Bool
}

func (a *toggledHandoverAuthorizer) exchange(ctx context.Context, r directCodeRequest) (directGrant, error) {
	if r.Handover && !a.allow.Load() {
		return directGrant{}, broker.HandoverRefusal{Reason: "unsupported-mode"}
	}
	return a.deviceAuthClient.exchange(ctx, r)
}
func TestRefusedHandoverPendingFollowsSuccessfulOtherConversation(t *testing.T) {
	r := newDetachedRuntime(t)
	initial := claudeHandoverIdentity(830, 10401)
	next := claudeHandoverIdentity(831, initial.PID)
	roster := &followRoster{id: initial}
	clock := &followClock{now: time.Unix(1000, 0), ticks: make(chan followTick, 1)}
	native := followNativeInbox(t, initial)
	var allow atomic.Bool
	var b *ingressBinding
	h := r.identityHarness(t, t.TempDir(), initial, func(o *durableOptions) {
		followOptions(roster)(o)
		o.claudeFollowClock = clock.clock()
		o.device.authorizer = &toggledHandoverAuthorizer{o.device.authorizer.(*deviceAuthClient), &allow}
		o.claudeInboxFactory = newClaudeInboxFactory(t.Context(), initial, roster.observe, o.parentProcessKey)
		o.bindingCreated = func(binding *ingressBinding) {
			if binding.identity.SessionID == initial.SessionID {
				b = binding
			}
		}
	})
	h.attach(t)
	manager := r.harness(t, t.TempDir(), 10402)
	manager.attach(t)
	roster.set(next)
	if err := call(t.Context(), manager.client, "send_message", broker.Message{From: manager.session.Name, To: h.session.Name, Body: "follow-other"}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "refusal holds pending", func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.followStopped && b.followMessage != nil })
	allow.Store(true)
	roster.set(claudeHandoverIdentity(832, initial.PID))
	select {
	case tick := <-clock.ticks:
		close(tick.release)
	case <-time.After(time.Second):
		t.Fatal("poll missing")
	}
	select {
	case m := <-native:
		if m.Body != "follow-other" {
			t.Fatal("wrong pending message")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending message did not follow successful new target")
	}
	waitFor(t, "retired source follow state cleared", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return !b.followStopped && b.followMessage == nil
	})
	select {
	case <-native:
		t.Fatal("duplicate pending delivery")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRefusedHandoverGateUsesSessionFileAndRestoresOnReturn(t *testing.T) {
	f, opts, id, _ := fastFollowFixture(t)
	b := f.lookup(id.SessionID)
	target := claudeHandoverIdentity(840, id.PID)
	current := target
	now := time.Unix(1000, 0)
	opts.claudeFollowClock = devicePollClock{now: func() time.Time { return now }, sleep: func(context.Context, time.Duration) error { return nil }}
	opts.claudeSessionFile = func(int) ([]byte, error) {
		return json.Marshal(map[string]any{"pid": current.PID, "cwd": current.CWD, "kind": "bg", "sessionId": current.SessionID})
	}
	observed := 0
	opts.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) {
		observed++
		return current, nil
	}
	f.retryID = target.SessionID
	f.retryAt = now.Add(10 * time.Minute)
	f.move = func(context.Context, broker.AttachRequest, broker.AttachRequest) (broker.AttachRequest, error) {
		t.Fatal("refused target retried before gate expired")
		return id, nil
	}
	b.followStopped = true
	b.failed = true
	b.receiveUnavailable = true
	b.receiveReason = "reconnect-required"
	b.followMessage = &broker.Message{Body: "pending"}
	restored, delivered := 0, 0
	f.restore = func(context.Context, *ingressBinding) error {
		restored++
		b.failed = false
		b.receiveUnavailable = false
		return nil
	}
	b.sink = func(context.Context, broker.Message) error { delivered++; return nil }
	for i := 0; i < 10; i++ {
		_, _ = f.check(t.Context(), id)
	}
	if observed != 0 {
		t.Errorf("active refusal gate invoked observe %d times; want 0", observed)
	}
	if restored != 0 || delivered != 0 {
		t.Fatal("source resumed while conversation was refused target")
	}
	current = id
	_, _ = f.check(t.Context(), id)
	if restored != 1 || delivered != 1 || b.followStopped || b.followMessage != nil || b.receiveReason != "" {
		t.Fatal("session-file return did not restore source on next check")
	}
}

func TestRefusedHandoverFastGateFallsBack(t *testing.T) {
	for _, kind := range []string{"missing", "corrupt", "pid", "kind", "cwd", "changed", "expired"} {
		t.Run(kind, func(t *testing.T) {
			f, opts, id, _ := fastFollowFixture(t)
			target := claudeHandoverIdentity(850, id.PID)
			current := target
			now := time.Unix(1000, 0)
			opts.claudeFollowClock = devicePollClock{now: func() time.Time { return now }, sleep: func(context.Context, time.Duration) error { return nil }}
			f.retryID = target.SessionID
			f.retryAt = now.Add(10 * time.Minute)
			if kind == "expired" {
				f.retryAt = now
			}
			if kind == "changed" {
				current = claudeHandoverIdentity(851, id.PID)
			}
			entry := map[string]any{"pid": current.PID, "cwd": current.CWD, "kind": "interactive", "sessionId": current.SessionID}
			switch kind {
			case "pid":
				entry["pid"] = current.PID + 1
			case "kind":
				entry["kind"] = "worker"
			case "cwd":
				entry["cwd"] = "/other"
			}
			opts.claudeSessionFile = func(int) ([]byte, error) {
				if kind == "missing" {
					return nil, errors.New("missing synthetic file")
				}
				if kind == "corrupt" {
					return []byte("{"), nil
				}
				return json.Marshal(entry)
			}
			observations, moves := 0, 0
			opts.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) {
				observations++
				return current, nil
			}
			f.move = func(context.Context, broker.AttachRequest, broker.AttachRequest) (broker.AttachRequest, error) {
				moves++
				return current, broker.HandoverRefusal{Reason: "unsupported-mode"}
			}
			_, _ = f.check(t.Context(), id)
			if observations == 0 {
				t.Fatal("invalid, changed or expired session-file gate skipped full observation")
			}
			if (kind == "changed" || kind == "expired") && (moves != 1 || observations < 8) {
				t.Fatal("full follow/stability path was lost")
			}
			if kind != "changed" && kind != "expired" && moves != 0 {
				t.Fatal("invalid file bypassed active retry gate after observation")
			}
		})
	}
}
