package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExplicitLiveL1SameClientAfterCleanup(t *testing.T) {
	r := newDetachedRuntime(t)
	manager := r.harness(t, t.TempDir(), 1801)
	manager.attach(t)
	made := make(chan *ingressBinding, 1)
	target := r.harness(t, t.TempDir(), 1802, func(o *durableOptions) { o.bindingCreated = func(b *ingressBinding) { made <- b } })
	target.attach(t)
	b := <-made
	old := target.session
	if err := call(t.Context(), manager.client, "stop_session", map[string]string{"name": old.RegistrationID}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "live receiver observed detach", func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.failed && b.durable.detached })
	cleanupPanelList(t, r)
	before := r.direct.Load()
	target.attach(t)
	if target.session.RegistrationID == old.RegistrationID || target.session.Name != old.Name {
		t.Fatal("L1 did not replace registration with same name")
	}
	if r.direct.Load()-before != 2 {
		t.Fatal("L1 needs N1 not-found plus N2")
	}
	target.attach(t)
	if r.direct.Load()-before != 2 {
		t.Fatal("L6 live attach reissued admission")
	}
	b.durable.session.mu.Lock()
	permits := b.durable.session.explicitAttaching
	b.durable.session.mu.Unlock()
	if permits != 0 {
		t.Fatal("explicit permit leaked")
	}
	if err := call(t.Context(), manager.client, "get_session", map[string]string{"name": target.session.Name}, new(broker.Session)); err != nil {
		t.Fatal(err)
	}
	if err := call(t.Context(), manager.client, "send_message", map[string]string{"from": manager.session.Name, "to": target.session.Name, "body": "L1-peer"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-target.received:
		if m.Body != "L1-peer" {
			t.Fatal("wrong peer body")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("L1 peer receipt")
	}
	select {
	case <-target.received:
		t.Fatal("duplicate peer receipt")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestExplicitLiveA5StoppedBindingWithClosedCachedSession(t *testing.T) {
	r := newDetachedRuntime(t)
	made := make(chan *ingressBinding, 1)
	connections := make(chan *brokerUpstream, 2)
	h := r.harness(t, t.TempDir(), 1890, func(o *durableOptions) {
		o.bindingCreated = func(b *ingressBinding) { made <- b }
		original := o.device.newUpstream
		o.device.newUpstream = func(token string) Upstream {
			u := original(token).(*brokerUpstream)
			connections <- u
			return u
		}
	})
	h.attach(t)
	b, raw := <-made, <-connections
	old := h.session.RegistrationID
	if err := call(t.Context(), h.client, "stop_session", map[string]string{"name": h.session.Name}, nil); err != nil {
		t.Fatal("self stop failed")
	}
	b.mu.Lock()
	failed, done := b.failed, b.done
	b.mu.Unlock()
	if !failed {
		t.Fatal("stop did not retire binding")
	}
	select {
	case <-done:
	default:
		t.Fatal("stop did not join receiver")
	}
	// Fix the cancellation race: a receiver can leave its cached MCP session closed.
	raw.mu.Lock()
	cached := raw.current
	raw.mu.Unlock()
	if cached == nil {
		t.Fatal("missing cached connection")
	}
	if err := cached.Close(); err != nil {
		t.Fatal("close cached connection")
	}
	result, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		class := "other"
		if result != nil {
			for _, c := range result.Content {
				if c, ok := c.(*mcp.TextContent); ok {
					for _, known := range []string{"broker get_session transport failed", "session_token_info", "ingress binding changed", "ingress attachment identity mismatch", "ingress registration was retired", "credential-rejected", "insufficient-scope"} {
						if strings.Contains(c.Text, known) {
							class = known
							break
						}
					}
				}
			}
		}
		t.Fatalf("closed cached session blocked explicit recovery: class=%s transportError=%t", class, err != nil)
	}
	if h.status(t).RegistrationID != old || r.direct.Load() != 1 {
		t.Fatal("recovery duplicated registration or admission")
	}
	b.durable.session.mu.Lock()
	permits := b.durable.session.explicitAttaching
	b.durable.session.mu.Unlock()
	if permits != 0 {
		t.Fatal("explicit permit leaked")
	}
}

func TestExplicitWrapperL9PermitLifecycle(t *testing.T) {
	for _, mode := range []string{"retry", "second-stale", "identify-failure", "prepare-timeout"} {
		t.Run(mode, func(t *testing.T) {
			server := mcp.NewServer(&mcp.Implementation{Name: "explicit-wrapper", Version: "test"}, nil)
			permits, preflights, prepares, registers, identifies := 0, 0, 0, 0, 0
			preflight := func(context.Context, *mcp.CallToolRequest) (func(context.Context) error, func(), error) {
				preflights++
				permits++
				return func(context.Context) error {
					prepares++
					if mode == "prepare-timeout" {
						return context.DeadlineExceeded
					}
					return nil
				}, func() { permits-- }, nil
			}
			identify := func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
				identifies++
				if mode == "identify-failure" {
					return broker.AttachRequest{}, errors.New("identity unavailable")
				}
				return broker.AttachRequest{SessionID: "U", ChannelID: fmt.Sprint(prepares)}, nil
			}
			registration := func(_ context.Context, id broker.AttachRequest) (broker.Session, error) {
				if permits != 1 {
					t.Error("permit absent during logical explicit call")
				}
				registers++
				if registers == 1 || mode == "second-stale" {
					return broker.Session{}, errExplicitBindingStale
				}
				return broker.Session{ID: id.SessionID}, nil
			}
			attach := addClientToolsWithPolicy(server, unavailableUpstream{}, nil, identify, registration, true, nil, nil, preflight, nil, func(id broker.AttachRequest, s broker.Session) error {
				if id.ChannelID != fmt.Sprint(prepares) {
					t.Error("notification got stale identity")
				}
				return nil
			})
			st, ct := mcp.NewInMemoryTransports()
			ss, err := server.Connect(t.Context(), st, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ss.Close()
			c, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(t.Context(), ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			result, err := c.CallTool(t.Context(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
			success := err == nil && !result.IsError
			if success != (mode == "retry") || permits != 0 || preflights != 1 {
				t.Fatalf("mode=%s success=%t permits=%d preflights=%d", mode, success, permits, preflights)
			}
			if mode == "retry" || mode == "second-stale" {
				if prepares != 2 || registers != 2 {
					t.Fatal("stale rerun not exactly once")
				}
			}
			// Automatic override must not acquire an explicit permit, even with inherited context.
			before := preflights
			_, _ = attach(context.WithValue(t.Context(), manualExplicitAttachKey{}, true), nil, "", func(context.Context, broker.AttachRequest) (broker.Session, error) {
				if permits != 0 {
					t.Error("automatic treated as explicit")
				}
				return broker.Session{}, nil
			})
			if preflights != before || permits != 0 {
				t.Fatal("automatic entered explicit preflight")
			}
		})
	}
}

func TestExplicitLiveL2RecoveryJoinAndDeadline(t *testing.T) {
	for _, mode := range []string{"before-exchange", "backoff", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			r := newDetachedRuntime(t)
			made := make(chan *ingressBinding, 1)
			h := r.harness(t, t.TempDir(), 1810, func(o *durableOptions) { o.bindingCreated = func(b *ingressBinding) { made <- b } })
			h.attach(t)
			b := <-made
			a := b.durable.session
			recoveryCtx, cancelRecovery := context.WithCancel(t.Context())
			done := make(chan struct{})
			release := make(chan struct{})
			entered := make(chan struct{})
			a.mu.Lock()
			a.recovering, a.recoveryDone, a.recoveryCancel = true, done, cancelRecovery
			a.mu.Unlock()
			go func() {
				if mode == "backoff" {
					a.exchangeMu.Lock()
					defer a.exchangeMu.Unlock()
				}
				close(entered)
				<-recoveryCtx.Done()
				<-release
				a.mu.Lock()
				a.recovering = false
				close(done)
				a.mu.Unlock()
			}()
			<-entered
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			if mode == "timeout" {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 30*time.Millisecond)
			}
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- call(ctx, h.client, "attach_session", map[string]any{}, nil) }()
			select {
			case <-recoveryCtx.Done():
			case <-time.After(time.Second):
				t.Fatal("recovery not canceled")
			}
			if mode == "timeout" {
				if err := <-result; err == nil {
					t.Error("deadline accepted")
				}
				close(release)
				<-done
				waitFor(t, "timeout releases permit", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.explicitAttaching == 0 })
				got, e := h.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
				if e != nil {
					t.Fatal("later attach transport", e)
				}
				if got.IsError {
					a.mu.Lock()
					state := a.status
					explicit := a.explicitAttaching
					a.mu.Unlock()
					identityMismatch := false
					for _, item := range got.Content {
						if text, ok := item.(*mcp.TextContent); ok {
							identityMismatch = identityMismatch || strings.Contains(text.Text, "ingress attachment identity mismatch")
						}
					}
					t.Fatalf("later attach rejected status=%s permits=%d identityMismatch=%t", state, explicit, identityMismatch)
				}
			} else {
				select {
				case <-result:
					t.Fatal("attach passed unfinished recovery")
				default:
				}
				close(release)
				if err := <-result; err != nil {
					t.Fatal(err)
				}
			}
			a.mu.Lock()
			permits := a.explicitAttaching
			a.mu.Unlock()
			if permits != 0 {
				t.Fatal("L2 permit leak")
			}
		})
	}
}

func TestExplicitLiveL3StoppedAutomaticPaths(t *testing.T) {
	r := newDetachedRuntime(t)
	manager := r.harness(t, t.TempDir(), 1820)
	manager.attach(t)
	made := make(chan *ingressBinding, 1)
	h := r.harness(t, t.TempDir(), 1821, func(o *durableOptions) { o.bindingCreated = func(b *ingressBinding) { made <- b } })
	h.attach(t)
	b := <-made
	if err := call(t.Context(), manager.client, "stop_session", map[string]string{"name": h.session.RegistrationID}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stopped receiver", func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.failed && b.durable.detached })
	cleanupPanelList(t, r)
	before := r.direct.Load()
	for i := 0; i < 12; i++ {
		h.status(t)
		_ = call(t.Context(), h.client, "list_sessions", map[string]any{}, nil)
		_ = call(t.Context(), h.client, "ingress_hook", map[string]any{"session_id": h.session.ID, "hook_event_name": "UserPromptSubmit"}, nil)
	}
	if r.direct.Load() != before {
		t.Fatal("stopped hook/helper/auth re-admitted conversation")
	}
	a := b.durable.session
	a.mu.Lock()
	permits := a.explicitAttaching
	a.mu.Unlock()
	if permits != 0 {
		t.Fatal("automatic paths acquired explicit permit")
	}
}

func TestExplicitSteerL5RegistrationBoundary(t *testing.T) {
	for _, mode := range []string{"new-registration", "new-epoch", "same-lease", "inflight"} {
		t.Run(mode, func(t *testing.T) {
			f := &fakeSteerControl{result: context.DeadlineExceeded}
			r, pushed, completed := steerFixture(t, f)
			diagnostics := 0
			r.steer.diagnose = func(code string) {
				if code == "steer-uncertain-retired" {
					diagnostics++
				}
			}
			if mode == "inflight" {
				f.release = make(chan struct{})
				f.entered = make(chan struct{}, 1)
				done := make(chan error, 1)
				go func() { done <- steerMessage(r, completed, 1) }()
				<-f.entered
				r.nextGeneration()
				close(f.release)
				<-done
			} else {
				steerMessage(r, completed, 1)
				r.nextGeneration()
			}
			reg, epoch := "R2", "E"
			if mode == "new-epoch" {
				reg, epoch = "R", "E2"
			}
			if mode == "same-lease" {
				reg = "R"
			}
			if r.bindRegistration(reg, epoch) {
				r.steer.diagnose("steer-uncertain-retired")
			}
			if *completed != 0 || *pushed != 0 {
				t.Fatal("old batch completed or resent")
			}
			if mode == "same-lease" {
				if r.steer.batch == nil || diagnostics != 0 {
					t.Fatal("same registration lost interception")
				}
				steerMessage(r, completed, 1)
				f.mu.Lock()
				f.found = true
				f.mu.Unlock()
				r.steerDecision(t.Context(), false)
				if *completed != 1 {
					t.Fatal("same-R replay not completed once")
				}
			} else {
				if r.steer.batch != nil || r.owner != "" || diagnostics != 1 {
					t.Fatal("new registration FIFO still blocked")
				}
				if r.bindRegistration(reg, epoch) {
					r.steer.diagnose("steer-uncertain-retired")
				}
				if diagnostics != 1 {
					t.Fatal("duplicate retirement event")
				}
				f.mu.Lock()
				f.result = nil
				f.mu.Unlock()
				writeCodexHook(r, "UserPromptSubmit", "T")
				ctx := context.WithValue(t.Context(), handoffKey{}, handoffCompletion{registration: reg, epoch: epoch, done: func() error { *completed++; return nil }})
				if err := r.deliver(ctx, broker.Message{RegistrationID: reg, DeliverySequence: 1, Body: "new"}); err != nil {
					t.Fatal(err)
				}
				if *completed != 1 || f.inputs != 2 {
					t.Fatal("new registration message not handed off once")
				}
			}
		})
	}
}

type explicitReadBarrier struct {
	Upstream
	before func(context.Context, *mcp.CallToolParams)
	after  func(context.Context, *mcp.CallToolParams, *mcp.CallToolResult, error)
}

func (u *explicitReadBarrier) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	u.before(ctx, p)
	result, err := u.Upstream.CallTool(ctx, p)
	if u.after != nil {
		u.after(ctx, p, result, err)
	}
	return result, err
}
func (u *explicitReadBarrier) credentialRejected() bool { return upstreamRejected(u.Upstream) }

func TestExplicitLiveL9LeaseChangesDuringRead(t *testing.T) {
	r := newDetachedRuntime(t)
	made := make(chan *ingressBinding, 1)
	var binding *ingressBinding
	var options durableOptions
	var armed atomic.Bool
	var reads atomic.Int32
	var holdWait atomic.Bool
	holdWait.Store(true)
	receiverHeld := make(chan struct{})
	var traceMu sync.Mutex
	aliases := map[string]string{}
	events := []map[string]any{}
	label := func(channel string) string {
		if channel == "" {
			return "empty"
		}
		if aliases[channel] == "" {
			aliases[channel] = fmt.Sprintf("C%d", len(aliases))
		}
		return aliases[channel]
	}
	record := func(method string, result *mcp.CallToolResult, callErr error) {
		if binding == nil {
			return
		}
		binding.mu.Lock()
		id, gen, started, failed := binding.identity, binding.generation, binding.started, binding.failed
		binding.mu.Unlock()
		var reply struct {
			ID         string
			Generation uint64
			Code       string
			Session    broker.Session
		}
		if result != nil {
			raw, _ := json.Marshal(result.StructuredContent)
			_ = json.Unmarshal(raw, &reply)
		}
		if reply.Session.ID != "" {
			reply.ID, reply.Generation = reply.Session.ID, reply.Session.Generation
		}
		code := reply.Code
		switch code {
		case "", "not-found", "detached", "owned", "registration-in-use", "revision-mismatch", "invalid", "storage", "suspended":
		default:
			code = "other"
		}
		traceMu.Lock()
		defer traceMu.Unlock()
		events = append(events, map[string]any{"method": method, "channel": label(id.ChannelID), "generation": gen, "started": started, "failed": failed, "sessionMatch": reply.ID == id.SessionID, "returnedGeneration": reply.Generation, "code": code, "transportError": callErr != nil})
	}
	h := r.harness(t, t.TempDir(), 1830, func(o *durableOptions) {
		// Isolate one lease change; the live old receiver must not start a second recovery.
		o.heartbeat = time.Hour
		o.bindingCreated = func(b *ingressBinding) { made <- b }
		original := o.device.newUpstream
		o.device.newUpstream = func(token string) Upstream {
			raw := original(token)
			return &explicitReadBarrier{Upstream: raw, before: func(ctx context.Context, p *mcp.CallToolParams) {
				if p.Name == "wait_for_message" && holdWait.Swap(false) {
					close(receiverHeld)
					<-ctx.Done()
				}
				if p.Name == "get_session" && (armed.Load() || reads.Load() > 0) {
					record("before-get", nil, nil)
				}
				if p.Name == "get_session" && armed.Load() && reads.Add(1) == 2 && armed.Swap(false) {
					if _, ok := resumeDurable(ctx, raw, binding, options, "barrier-new-lease"); !ok {
						t.Error("lease barrier failed")
					}
					record("injected-resume", nil, nil)
				}
			}, after: func(_ context.Context, p *mcp.CallToolParams, result *mcp.CallToolResult, err error) {
				if armed.Load() || reads.Load() > 0 {
					if p.Name == "get_session" || p.Name == "resume_session" {
						record(p.Name, result, err)
					}
				}
			}}
		}
		options = *o
	})
	h.attach(t)
	binding = <-made
	select {
	case <-receiverHeld:
	case <-time.After(3 * time.Second):
		t.Fatal("old receiver did not reach the lease barrier")
	}
	binding.mu.Lock()
	old := binding.identity.ChannelID
	initialGeneration := binding.generation
	binding.mu.Unlock()
	traceMu.Lock()
	label(old)
	traceMu.Unlock()
	armed.Store(true)
	result, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
	record("assertion", result, err)
	class := "ok"
	if err != nil {
		class = "transport"
		if errors.Is(err, context.Canceled) {
			class = "cancelled"
		}
		if errors.Is(err, context.DeadlineExceeded) {
			class = "deadline"
		}
	}
	if result != nil && result.IsError {
		class = "other-rejection"
		for _, content := range result.Content {
			if text, ok := content.(*mcp.TextContent); ok {
				for _, known := range []struct{ message, class string }{{"ingress binding changed; retry attach_session", "explicit-binding-stale"}, {"ingress attachment identity mismatch", "attached-identity-mismatch"}, {"ingress registration was retired", "retired-binding"}, {"broker get_session transport failed", "get-transport"}, {"invalid registration proof", "invalid-proof"}, {"credential-rejected", "credential-rejected"}, {"direct-registration-failed", "direct-registration-failed"}} {
					if strings.Contains(text.Text, known.message) {
						class = known.class
						break
					}
				}
			}
		}
	}
	traceMu.Lock()
	snapshot := append([]map[string]any(nil), events...)
	traceMu.Unlock()
	if err != nil || result == nil || result.IsError {
		t.Fatalf("L9 mcpClass=%s reads=%d trace=%v", class, reads.Load(), snapshot)
	}
	t.Logf("L9 mcpClass=%s reads=%d trace=%v", class, reads.Load(), snapshot)
	binding.mu.Lock()
	changed := binding.identity.ChannelID != old
	finalGeneration := binding.generation
	binding.mu.Unlock()
	if !changed {
		t.Fatal("lease barrier did not rotate")
	}
	if finalGeneration != initialGeneration+1 {
		t.Fatal("fixture introduced more than one lease change")
	}
	getCalls := 0
	for _, event := range snapshot {
		if event["method"] == "before-get" {
			getCalls++
		}
	}
	if getCalls != 4 {
		t.Fatalf("single stale rerun needs four proof/final reads, got %d", getCalls)
	}
	binding.durable.session.mu.Lock()
	counter := binding.durable.session.explicitAttaching
	binding.durable.session.mu.Unlock()
	if counter != 0 {
		t.Fatal("permit leaked")
	}
}

func TestExplicitLiveL2TimeoutAtResetBoundary(t *testing.T) {
	for _, stage := range []string{"before-reset", "after-reset"} {
		t.Run(stage, func(t *testing.T) {
			r := newDetachedRuntime(t)
			manager := r.harness(t, t.TempDir(), 1840)
			manager.attach(t)
			made := make(chan *ingressBinding, 1)
			var armed atomic.Bool
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			h := r.harness(t, t.TempDir(), 1841, func(o *durableOptions) {
				o.bindingCreated = func(b *ingressBinding) { made <- b }
				o.explicitAttachBarrier = func(serverCtx context.Context, point string) {
					if point == stage && armed.Swap(false) {
						cancel()
						select {
						case <-serverCtx.Done():
						case <-time.After(time.Second):
							t.Error("server cancellation not delivered to barrier")
						}
					}
				}
			})
			h.attach(t)
			b := <-made
			old := h.session
			if err := call(t.Context(), manager.client, "stop_session", map[string]string{"name": old.RegistrationID}, nil); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "boundary receiver detached", func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.failed && b.durable.detached })
			cleanupPanelList(t, r)
			b.mu.Lock()
			channel := b.identity.ChannelID
			b.mu.Unlock()
			armed.Store(true)
			result, err := h.client.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
			if err == nil && !result.IsError {
				t.Fatal("canceled boundary attach succeeded")
			}
			a := b.durable.session
			waitFor(t, "boundary permit released", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.explicitAttaching == 0 })
			if armed.Load() {
				t.Fatal("boundary not reached")
			}
			b.mu.Lock()
			rotated := b.identity.ChannelID != channel
			started, failed := b.started, b.failed
			b.mu.Unlock()
			if stage == "before-reset" && (rotated || !failed || !started) {
				t.Fatal("timeout before reset mutated binding")
			}
			if stage == "after-reset" && (!rotated || failed || started) {
				t.Fatal("timeout after reset inconsistent")
			}
			h.attach(t)
			if h.session.Name != old.Name || !h.status(t).Registered {
				t.Fatal("later attach could not use fresh identity")
			}
		})
	}
}

func TestExplicitLiveL4ProofJoinsUnobservedReceiver(t *testing.T) {
	r := newDetachedRuntime(t)
	manager := r.harness(t, t.TempDir(), 1850)
	manager.attach(t)
	entered := make(chan struct{}, 1)
	made := make(chan *ingressBinding, 1)
	var hold atomic.Bool
	hold.Store(true)
	h := r.harness(t, t.TempDir(), 1851, func(o *durableOptions) {
		o.heartbeat = time.Hour
		o.bindingCreated = func(b *ingressBinding) { made <- b }
		original := o.device.newUpstream
		o.device.newUpstream = func(token string) Upstream {
			return &explicitReadBarrier{Upstream: original(token), before: func(ctx context.Context, p *mcp.CallToolParams) {
				if p.Name == "wait_for_message" && hold.Swap(false) {
					entered <- struct{}{}
					<-ctx.Done()
				}
			}}
		}
	})
	h.attach(t)
	b := <-made
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("old receiver barrier")
	}
	old := h.session
	if err := call(t.Context(), manager.client, "stop_session", map[string]string{"name": old.RegistrationID}, nil); err != nil {
		t.Fatal(err)
	}
	cleanupPanelList(t, r)
	b.mu.Lock()
	started, failed, done := b.started, b.failed, b.done
	b.mu.Unlock()
	if !started || failed {
		t.Fatal("receiver observed stop before proof")
	}
	h.attach(t)
	select {
	case <-done:
	default:
		t.Fatal("new receiver overlaps old receiver")
	}
	if h.session.Name != old.Name || h.session.RegistrationID == old.RegistrationID {
		t.Fatal("L4 replacement failed")
	}
}

type explicitReadmitRefusal struct {
	deviceAuthorizer
	directAuthorizer
	deny  *atomic.Bool
	fresh *atomic.Int32
}

func (a explicitReadmitRefusal) exchange(ctx context.Context, r directCodeRequest) (directGrant, error) {
	if a.deny.Load() && r.RegistrationID == "" {
		a.fresh.Add(1)
		return directGrant{}, errInsufficientScope
	}
	return a.directAuthorizer.exchange(ctx, r)
}
func TestExplicitLiveL7ReadmissionRefusalStops(t *testing.T) {
	r := newDetachedRuntime(t)
	manager := r.harness(t, t.TempDir(), 1860)
	manager.attach(t)
	made := make(chan *ingressBinding, 1)
	var deny atomic.Bool
	var fresh atomic.Int32
	h := r.harness(t, t.TempDir(), 1861, func(o *durableOptions) {
		o.bindingCreated = func(b *ingressBinding) { made <- b }
		d := o.device.authorizer
		o.device.authorizer = explicitReadmitRefusal{d, d.(directAuthorizer), &deny, &fresh}
	})
	h.attach(t)
	b := <-made
	if err := call(t.Context(), manager.client, "stop_session", map[string]string{"name": h.session.RegistrationID}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "L7 detached", func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.failed && b.durable.detached })
	cleanupPanelList(t, r)
	deny.Store(true)
	result, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
	if err != nil || !result.IsError {
		t.Fatal("denied parent re-admitted")
	}
	text := ""
	for _, c := range result.Content {
		if c, ok := c.(*mcp.TextContent); ok {
			text += c.Text
		}
	}
	if !strings.Contains(text, "reattach-refused") || fresh.Load() != 1 {
		t.Fatal("L7 fixed refusal absent")
	}
	before := r.direct.Load()
	for i := 0; i < 4; i++ {
		h.status(t)
	}
	if r.direct.Load() != before || fresh.Load() != 1 {
		t.Fatal("L7 failed into automatic loop")
	}
}

func TestExplicitLiveL1ClaudeDeferredActivation(t *testing.T) {
	r := newDetachedRuntime(t)
	manager := r.harness(t, t.TempDir(), 1870)
	manager.attach(t)
	made := make(chan *ingressBinding, 1)
	messages := make(chan broker.Message, 2)
	id := broker.AttachRequest{Harness: "claude", SessionID: "11111111-1111-4111-8111-000000001871", PID: 1871, CWD: "/synthetic/detached"}
	h := r.identityHarness(t, t.TempDir(), id, func(o *durableOptions) {
		o.bindingCreated = func(b *ingressBinding) { made <- b }
		o.claudeInboxFactory = func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
			return func(_ context.Context, m broker.Message) error { messages <- m; return nil }, nil, nil
		}
	})
	h.attach(t)
	b := <-made
	old := h.session
	if err := call(t.Context(), manager.client, "stop_session", map[string]string{"name": old.RegistrationID}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Claude deferred detach", func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.failed && b.durable.detached })
	cleanupPanelList(t, r)
	h.attach(t)
	if h.session.Name != old.Name || h.session.RegistrationID == old.RegistrationID || !h.status(t).Registered {
		t.Fatal("Claude deferred re-entry failed")
	}
	if err := call(t.Context(), manager.client, "get_session", map[string]string{"name": h.session.Name}, new(broker.Session)); err != nil {
		t.Fatal(err)
	}
	if err := call(t.Context(), manager.client, "send_message", map[string]string{"from": manager.session.Name, "to": h.session.Name, "body": "Claude-L1"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-messages:
	case <-time.After(2 * time.Second):
		t.Fatal("Claude deferred receiver not activated")
	}
	a := b.durable.session
	a.mu.Lock()
	n := a.explicitAttaching
	a.mu.Unlock()
	if n != 0 {
		t.Fatal("Claude permit leaked")
	}
}

type explicitLateRecovery struct {
	deviceAuthorizer
	directAuthorizer
	armed            *atomic.Bool
	entered, release chan struct{}
}

func (a explicitLateRecovery) exchange(ctx context.Context, r directCodeRequest) (directGrant, error) {
	grant, err := a.directAuthorizer.exchange(ctx, r)
	if r.Recovery && a.armed.Swap(false) {
		close(a.entered)
		<-ctx.Done()
		<-a.release
	}
	return grant, err
}
func TestExplicitLiveL8LateAutomaticReply(t *testing.T) {
	r := newDetachedRuntime(t)
	manager := r.harness(t, t.TempDir(), 1880)
	manager.attach(t)
	made := make(chan *ingressBinding, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	var armed, hold atomic.Bool
	hold.Store(true)
	h := r.harness(t, t.TempDir(), 1881, func(o *durableOptions) {
		o.heartbeat = time.Hour
		o.bindingCreated = func(b *ingressBinding) { made <- b }
		d := o.device.authorizer
		o.device.authorizer = explicitLateRecovery{d, d.(directAuthorizer), &armed, entered, release}
		original := o.device.newUpstream
		o.device.newUpstream = func(token string) Upstream {
			return &explicitReadBarrier{Upstream: original(token), before: func(ctx context.Context, p *mcp.CallToolParams) {
				if p.Name == "wait_for_message" && hold.Swap(false) {
					<-ctx.Done()
				}
			}}
		}
	})
	h.attach(t)
	b := <-made
	old := h.session
	if err := call(t.Context(), manager.client, "stop_session", map[string]string{"name": old.RegistrationID}, nil); err != nil {
		t.Fatal(err)
	}
	cleanupPanelList(t, r)
	armed.Store(true)
	h.status(t)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("late recovery barrier not entered")
	}
	b.mu.Lock()
	channel := b.identity.ChannelID
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	result, err := h.client.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
	if err == nil && !result.IsError {
		t.Error("attach passed unresolved old recovery")
	}
	close(release)
	a := b.durable.session
	waitFor(t, "late reply joined", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return !a.recovering && a.explicitAttaching == 0 })
	b.mu.Lock()
	unchanged := b.identity.ChannelID == channel
	b.mu.Unlock()
	if !unchanged {
		t.Fatal("late reply reset binding after timeout")
	}
	h.attach(t)
	if h.session.RegistrationID == old.RegistrationID || h.session.Name != old.Name {
		t.Fatal("late old reply survived explicit replacement")
	}
}
