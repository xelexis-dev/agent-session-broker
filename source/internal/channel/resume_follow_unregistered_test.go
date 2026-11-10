package channel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestResumeFollowUnregisteredPollResumesWithoutToolCall(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	savedID := claudeHandoverIdentity(501, 7101)
	var oldBinding *ingressBinding
	old := r.identityHarness(t, dir, savedID, func(o *durableOptions) {
		handoverOptions(o)
		o.heartbeat = time.Hour
		o.bindingCreated = func(b *ingressBinding) { oldBinding = b }
	})
	old.attach(t)
	original := old.session
	_, _, saved := handoverSaved(t, r, dir, savedID)
	oldBinding.mu.Lock()
	lease := oldBinding.identity.ChannelID
	oldBinding.mu.Unlock()
	probe := newTestBrokerUpstream(t.Context(), r.endpoint)
	probe.setBearerToken(saved.Credential.AccessToken)
	if err := call(t.Context(), probe, "report_ingress_failure", map[string]string{"sessionId": savedID.SessionID, "registrationId": original.RegistrationID, "channelId": lease}, nil); err != nil {
		t.Fatal("fixture could not mark old ingress missing")
	}
	probe.Close()
	old.close()
	initial := claudeHandoverIdentity(502, 7102)
	current := savedID
	current.PID = initial.PID
	roster := &followRoster{id: initial}
	clock := &followClock{now: time.Unix(1000, 0), ticks: make(chan followTick, 1)}
	received := followNativeInbox(t, initial)
	created := make(chan struct{})
	var once sync.Once
	h := r.identityHarness(t, dir, initial, func(o *durableOptions) {
		followOptions(roster)(o)
		o.claudeFollowClock = clock.clock()
		o.claudeInboxFactory = newClaudeInboxFactory(t.Context(), initial, roster.observe, o.parentProcessKey)
		o.bindingCreated = func(*ingressBinding) { once.Do(func() { close(created) }) }
	})

	_ = h // No tool call is made through this client after initialization.
	select {
	case <-created:
	case <-time.After(time.Second):
		t.Fatal("startup binding missing")
	}
	time.Sleep(100 * time.Millisecond) // Existing startup retries finish against T; monitor remains clock-gated.
	manager := r.harness(t, t.TempDir(), 7103)
	manager.attach(t)
	roster.set(current)
	select {
	case tick := <-clock.ticks:
		if tick.delay != 2*time.Second {
			t.Fatal("startup interval differs")
		}
		close(tick.release)
	case <-time.After(time.Second):
		t.Fatal("follow monitor missing")
	}
	waitFor(t, "poll alone resumes saved registration", func() bool {
		var remote broker.Session
		return call(context.Background(), manager.client, "get_session", map[string]string{"name": original.RegistrationID}, &remote) == nil && remote.PID == initial.PID && remote.Revision > original.Revision && remote.AutomaticDeliveryReady
	})
	if err := call(t.Context(), manager.client, "send_message", broker.Message{From: manager.session.Name, To: original.Name, Body: "unregistered-follow"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-received:
		if m.Body != "unregistered-follow" {
			t.Fatal("wrong native message")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("native inbox delivery missing")
	}
}

func unregisteredMonitor(t *testing.T, r *detachedRuntime, dir string, id broker.AttachRequest, roster *followRoster, clock *followClock, configure ...func(*durableOptions)) (*detachedHarness, *claudeFollow) {
	t.Helper()
	created := make(chan struct{})
	var once sync.Once
	var options *durableOptions
	h := r.identityHarness(t, dir, id, func(o *durableOptions) {
		options = o
		followOptions(roster)(o)
		o.claudeFollowClock = clock.clock()
		o.bindingCreated = func(*ingressBinding) { once.Do(func() { close(created) }) }
		for _, apply := range configure {
			apply(o)
		}
	})
	select {
	case <-created:
	case <-time.After(time.Second):
		t.Fatal("startup binding unavailable")
	}
	time.Sleep(100 * time.Millisecond)
	return h, options.claudeFollow
}
func unregisteredStep(t *testing.T, clock *followClock) time.Duration {
	t.Helper()
	var tick followTick
	select {
	case tick = <-clock.ticks:
	case <-time.After(time.Second):
		t.Fatal("monitor tick unavailable")
	}
	close(tick.release)
	waitFor(t, "monitor check completed", func() bool { return len(clock.ticks) > 0 })
	return tick.delay
}
func TestResumeFollowUnregisteredUnsavedConversationDoesNotAdmit(t *testing.T) {
	r := newDetachedRuntime(t)
	initial := claudeHandoverIdentity(510, 7201)
	roster := &followRoster{id: initial}
	clock := &followClock{now: time.Unix(1000, 0), ticks: make(chan followTick, 1)}
	_, f := unregisteredMonitor(t, r, t.TempDir(), initial, roster, clock)
	roster.set(claudeHandoverIdentity(511, 7201))
	for i := 0; i < 3; i++ {
		unregisteredStep(t, clock)
	}
	if r.direct.Load() != 0 || r.resumes.Load() != 0 {
		t.Fatal("unsaved identity started admission or resume")
	}
	if f.summary() != "" {
		t.Fatal("unsaved identity was reported as a transition")
	}
}
func TestResumeFollowUnregisteredThirtySecondInterval(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	savedID := claudeHandoverIdentity(520, 7301)
	old := r.identityHarness(t, dir, savedID, handoverOptions)
	old.attach(t)
	original := old.session
	old.close()
	initial := claudeHandoverIdentity(521, 7301)
	roster := &followRoster{id: initial}
	clock := &followClock{now: time.Unix(1000, 0), ticks: make(chan followTick, 1)}
	path := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "-synthetic-handover", initial.SessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	_, _ = unregisteredMonitor(t, r, dir, initial, roster, clock)
	clock.mu.Lock()
	clock.now = clock.now.Add(3 * time.Minute)
	clock.mu.Unlock()
	if unregisteredStep(t, clock) != 2*time.Second {
		t.Fatal("initial pending tick differs")
	}
	roster.set(savedID)
	if unregisteredStep(t, clock) != 30*time.Second {
		t.Fatal("settled transcript did not select 30s interval")
	}
	manager := r.harness(t, t.TempDir(), 7302)
	manager.attach(t)
	var remote broker.Session
	if err := call(t.Context(), manager.client, "get_session", map[string]string{"name": original.RegistrationID}, &remote); err != nil || !remote.AutomaticDeliveryReady || remote.Revision <= original.Revision {
		t.Fatal("30-second poll did not resume")
	}
}
func TestResumeFollowUnregisteredHandedOverAndRefusedHolder(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(fmt.Sprint(refused), func(t *testing.T) {
			r := newDetachedRuntime(t)
			dir := t.TempDir()
			c0 := claudeHandoverIdentity(530, 7401)
			holderID := claudeHandoverIdentity(531, 7401)
			old := r.identityHarness(t, dir, c0, handoverOptions)
			old.attach(t)
			original := old.session
			old.close()
			holder := r.identityHarness(t, dir, holderID, handoverOptions)
			holder.attach(t)
			initial := claudeHandoverIdentity(532, 7401)
			if refused {
				initial.PID++
			} else {
				holder.close()
			}
			current := c0
			current.PID = initial.PID
			roster := &followRoster{id: initial}
			clock := &followClock{now: time.Unix(1000, 0), ticks: make(chan followTick, 1)}
			var fresh atomic.Int32
			_, f := unregisteredMonitor(t, r, dir, initial, roster, clock, func(o *durableOptions) {
				o.device.authorizer = &returnAuthorizer{o.device.authorizer.(*deviceAuthClient), &fresh}
			})
			roster.set(current)
			unregisteredStep(t, clock)
			if refused {
				if f.summary() != "follow-skipped: still-receiving" {
					t.Fatal("holder refusal not diagnosed")
				}
				attempts := r.direct.Load()
				for i := 0; i < 4; i++ {
					unregisteredStep(t, clock)
				}
				if r.direct.Load() != attempts {
					t.Fatal("refused holder retried every 2 seconds")
				}
			} else {
				if f.summary() != "followed: handover" {
					t.Fatal("handed-over identity was not reclaimed")
				}
				if b := f.lookup(current.SessionID); b == nil {
					t.Fatal("reclaimed binding absent")
				} else {
					b.mu.Lock()
					ok := b.started && !b.failed && b.durable.registrationID == original.RegistrationID
					b.mu.Unlock()
					if !ok {
						t.Fatal("reclaimed registration changed")
					}
				}
			}
			if fresh.Load() != 0 {
				t.Fatal("holder follow allocated fresh registration")
			}
		})
	}
}
func TestResumeFollowUnregisteredRetryGate(t *testing.T) {
	initial := claudeHandoverIdentity(540, 7501)
	next := claudeHandoverIdentity(541, 7501)
	roster := &followRoster{id: next}
	clock := instantClaudeClock()
	opts := durableOptions{device: &deviceAuthOptions{}, observe: roster.observe, claudeStartupClock: instantClaudeClock(), claudeFollowClock: clock, parentProcessKey: func(context.Context, int) (string, error) { return "same", nil }}
	f := newClaudeFollow(initial, &opts)
	f.lookup = func(string) *ingressBinding { return nil }
	f.noRegistered = func() bool { return true }
	f.move = func(context.Context, broker.AttachRequest, broker.AttachRequest) (broker.AttachRequest, error) {
		t.Fatal("registered move called")
		return initial, nil
	}
	attempts := 0
	f.startup = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) {
		attempts++
		return next, broker.HandoverRefusal{Reason: "suspended"}
	}
	_, _ = f.check(t.Context(), initial)
	for i := 0; i < 14; i++ {
		_ = clock.sleep(t.Context(), 2*time.Second)
		_, _ = f.check(t.Context(), initial)
	}
	if attempts != 1 {
		t.Fatal("refused destination retried before 30 seconds")
	}
	_ = clock.sleep(t.Context(), 2*time.Second)
	_, _ = f.check(t.Context(), initial)
	if attempts != 2 {
		t.Fatal("refused destination was not retried at 30 seconds")
	}
	roster.set(claudeHandoverIdentity(542, 7501))
	_, _ = f.check(t.Context(), initial)
	roster.set(next)
	_, _ = f.check(t.Context(), initial)
	if attempts != 4 {
		t.Fatal("conversation change did not reset retry gate")
	}
}
func TestResumeFollowUnregisteredObservationGuards(t *testing.T) {
	for _, kind := range []string{"error", "flap", "pid", "cwd", "key", "unsupported"} {
		t.Run(kind, func(t *testing.T) {
			initial := claudeHandoverIdentity(550, 7601)
			next := claudeHandoverIdentity(551, 7601)
			calls := 0
			opts := durableOptions{device: &deviceAuthOptions{}, claudeStartupClock: instantClaudeClock(), parentProcessKey: func(context.Context, int) (string, error) { return "same", nil }}
			opts.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) {
				calls++
				if kind == "error" {
					return next, errors.New("synthetic roster failure")
				}
				if kind == "flap" && calls%2 == 0 {
					return initial, nil
				}
				return next, nil
			}
			f := newClaudeFollow(initial, &opts)
			f.lookup = func(string) *ingressBinding { return nil }
			f.noRegistered = func() bool { return true }
			f.move = func(context.Context, broker.AttachRequest, broker.AttachRequest) (broker.AttachRequest, error) {
				t.Fatal("registered move called")
				return initial, nil
			}
			attempts := 0
			f.startup = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) {
				attempts++
				return next, nil
			}
			switch kind {
			case "pid":
				next.PID++
			case "cwd":
				next.CWD = "/changed"
			case "key":
				opts.parentProcessKey = func(context.Context, int) (string, error) { return "different", nil }
			case "unsupported":
				f.key = ""
			}
			_, _ = f.check(t.Context(), initial)
			if attempts != 0 {
				t.Fatal("unsafe unregistered observation initiated resume")
			}
		})
	}
}

func TestResumeFollowUnregisteredSavedTargetRefusalsAreThrottled(t *testing.T) {
	for _, reason := range []string{"suspended", "detached", "still-receiving"} {
		t.Run(reason, func(t *testing.T) {
			r := newDetachedRuntime(t)
			dir := t.TempDir()
			targetID := claudeHandoverIdentity(560, 7701)
			old := r.identityHarness(t, dir, targetID, handoverOptions)
			old.attach(t)
			manager := r.harness(t, t.TempDir(), 7703)
			manager.attach(t)
			if reason != "still-receiving" {
				tool := "suspend_session"
				if reason == "detached" {
					tool = "stop_session"
				}
				if err := call(t.Context(), manager.client, tool, map[string]string{"name": old.session.RegistrationID}, nil); err != nil {
					t.Fatal(err)
				}
				if reason == "detached" {
					waitFor(t, "saved detached marker", func() bool { _, _, s := handoverSaved(t, r, dir, targetID); return s.Registration.Detached })
				}
				old.close()
			}
			initial := claudeHandoverIdentity(561, 7702)
			current := targetID
			current.PID = initial.PID
			roster := &followRoster{id: initial}
			clock := &followClock{now: time.Unix(1000, 0), ticks: make(chan followTick, 1)}
			var probes atomic.Int32
			_, f := unregisteredMonitor(t, r, dir, initial, roster, clock, func(o *durableOptions) {
				original := o.device.newUpstream
				o.device.newUpstream = func(token string) Upstream { probes.Add(1); return original(token) }
			})
			direct, resumes := r.direct.Load(), r.resumes.Load()
			roster.set(current)
			unregisteredStep(t, clock)
			if f.summary() != "follow-skipped: "+reason {
				t.Fatal("saved target refusal not diagnosed")
			}
			firstProbes := probes.Load()
			for i := 0; i < 4; i++ {
				unregisteredStep(t, clock)
			}
			if probes.Load() != firstProbes || r.direct.Load() != direct || r.resumes.Load() != resumes {
				t.Fatal("saved refusal retried or admitted during cooldown")
			}
		})
	}
}

func TestResumeFollowUnregisteredKeepsLaterManualBinding(t *testing.T) {
	initial := claudeHandoverIdentity(570, 7801)
	unsaved := claudeHandoverIdentity(571, 7801)
	opts := durableOptions{device: &deviceAuthOptions{}, parentProcessKey: func(context.Context, int) (string, error) { return "same", nil }}
	opts.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return unsaved, nil }
	f := newClaudeFollow(initial, &opts)
	f.current = unsaved
	registered := &ingressBinding{mu: &sync.Mutex{}, started: true, identity: initial, durable: &durableRegistration{session: &sessionAuth{state: clientstate.SessionState{LastAdmissionMode: "manual"}}}}
	f.lookup = func(id string) *ingressBinding {
		if id == initial.SessionID {
			return registered
		}
		return nil
	}
	f.noRegistered = func() bool { return false }
	f.move = func(context.Context, broker.AttachRequest, broker.AttachRequest) (broker.AttachRequest, error) {
		t.Fatal("manual binding moved")
		return unsaved, nil
	}
	got, err := f.check(t.Context(), initial)
	if err != nil || got.SessionID != initial.SessionID {
		t.Fatal("earlier unregistered observation replaced the later manual binding")
	}
}
