package channel

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/localactivity"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func (u *switchableUpstream) verifyActivity(ctx context.Context) bool {
	// A restart can close this captured connection during schema discovery.
	// Match the real upstream: unavailable schema is unsupported, not Fatal
	// from the receiver goroutine (which would abort registration recovery).
	u.mu.Lock()
	client := u.client
	u.mu.Unlock()
	listed, err := client.ListTools(ctx, nil)
	return err == nil && activitySchemaAllowed(listed.Tools)
}
func (u *switchableUpstream) activityAllowed() bool { return !u.offline.Load() }

func (u *recordingUpstream) verifyActivity(ctx context.Context) bool {
	capable, ok := u.inner.(activityUpstream)
	return ok && capable.verifyActivity(ctx)
}
func (u *recordingUpstream) activityAllowed() bool {
	capable, ok := u.inner.(activityUpstream)
	return ok && !u.rejected.Load() && capable.activityAllowed()
}

type observerFunc func(context.Context, localactivity.Binding) localactivity.Observation

func (f observerFunc) Observe(ctx context.Context, b localactivity.Binding) localactivity.Observation {
	return f(ctx, b)
}

type activityRecorded struct {
	registration, lease string
	activity            *broker.Activity
}
type activityCapture struct {
	Upstream
	mu         sync.Mutex
	calls      []activityRecorded
	denySchema bool
	oldServer  bool
	resumes    atomic.Int32
}

func (u *activityCapture) verifyActivity(ctx context.Context) bool {
	if u.denySchema {
		return false
	}
	p, ok := u.Upstream.(activityUpstream)
	return ok && p.verifyActivity(ctx)
}
func (u *activityCapture) activityAllowed() bool {
	if u.denySchema {
		return false
	}
	p, ok := u.Upstream.(activityUpstream)
	return ok && p.activityAllowed()
}
func (u *activityCapture) snapshot() []activityRecorded {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]activityRecorded(nil), u.calls...)
}
func (u *activityCapture) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name == "resume_session" {
		u.resumes.Add(1)
	}
	if p.Name == "renew_lease" {
		raw, _ := json.Marshal(p.Arguments)
		var request broker.RenewRequest
		_ = json.Unmarshal(raw, &request)
		item := activityRecorded{registration: request.RegistrationID, lease: request.ChannelID}
		if len(request.Activity) > 0 {
			item.activity = &broker.Activity{}
			_ = json.Unmarshal(request.Activity, item.activity)
		}
		u.mu.Lock()
		u.calls = append(u.calls, item)
		u.mu.Unlock()
	}
	result, err := u.Upstream.CallTool(ctx, p)
	if err == nil && u.oldServer && (p.Name == "register_session" || p.Name == "resume_session" || p.Name == "renew_lease") {
		raw, _ := json.Marshal(result.StructuredContent)
		var data map[string]any
		_ = json.Unmarshal(raw, &data)
		delete(data, "activityVersion")
		copied := *result
		copied.StructuredContent = data
		result = &copied
	}
	return result, err
}

func TestActivityReconnectResetsSequence(t *testing.T) {
	auth := newDeviceAuthServer(t)
	up := newSwitchableUpstream(t)
	capture := &activityCapture{}
	f := newDeviceModeFixture(t, auth, up, t.TempDir(), singleIdentity(), func(f *deviceModeFixture) {
		f.wrapUpstream = func(_ string, inner Upstream) Upstream { capture.Upstream = inner; return capture }
	})
	const sid = "11111111-1111-4111-8111-111111111111"
	f.status(sid)
	f.grant(sid, "asb_s_a", "reconnect-activity")
	waitFor(t, "initial activity samples", func() bool {
		for _, c := range capture.snapshot() {
			if c.activity != nil && c.activity.Sequence >= 3 {
				return true
			}
		}
		return false
	})
	calls := capture.snapshot()
	oldLease := calls[len(calls)-1].lease
	before := capture.resumes.Load()
	up.offline.Store(true)
	waitFor(t, "automatic resume attempt", func() bool { return capture.resumes.Load() > before })
	up.offline.Store(false)
	waitFor(t, "new lease sample", func() bool {
		for _, c := range capture.snapshot() {
			if c.lease != oldLease && c.activity != nil {
				return c.activity.Sequence == 1
			}
		}
		return false
	})
	f.stop()
}

// Activity assertions inspect broker storage directly: these fixture queries
// have no authenticated caller binding for MCP output capability negotiation.
func TestActivityHTTPConnectionSchemaGate(t *testing.T) {
	up := newSwitchableUpstream(t)
	central := httptest.NewServer(mcpserver.Handler(mcpserver.New(up.service, noMemory{})))
	defer central.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	remote := newTestBrokerUpstream(ctx, central.URL+"/mcp")
	defer remote.Close()
	if remote.activityAllowed() {
		t.Fatal("schema trusted before handshake")
	}
	request := broker.RegisterRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 7, CWD: t.TempDir(), ClientID: "http-client", ChannelID: "http-lease", Secret: strings.Repeat("s", 40), Name: "http-activity", RequestID: "http-register"}
	var registered broker.RegisterResult
	if err := call(ctx, remote, "register_session", request, &registered); err != nil || registered.Code != "" {
		t.Fatal("HTTP registration failed")
	}
	raw, _ := json.Marshal(broker.Activity{State: broker.ActivityWorking, Source: "codex-rollout", Sequence: 1, Generation: registered.Session.Generation, ObservedAt: time.Now().UTC()})
	renew := broker.RenewRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: request.ChannelID, Activity: raw}
	var result broker.RegisterResult
	if err := call(ctx, remote, "renew_lease", renew, &result); err != nil {
		t.Fatal(err)
	}
	got, _ := up.service.GetSession(context.Background(), "http-activity")
	if got.Activity != nil && got.Activity.State == broker.ActivityWorking {
		t.Fatal("unverified connection sent activity")
	}
	if !remote.verifyActivity(ctx) || !remote.activityAllowed() {
		t.Fatal("actual renew schema not recognized")
	}
	if err := call(ctx, remote, "renew_lease", renew, &result); err != nil {
		t.Fatal(err)
	}
	got, _ = up.service.GetSession(context.Background(), "http-activity")
	if got.Activity == nil || got.Activity.State != broker.ActivityWorking {
		t.Fatal("verified connection lost activity")
	}
	remote.mu.Lock()
	old := remote.current
	remote.current = nil
	remote.mu.Unlock()
	if remote.activityAllowed() {
		t.Fatal("replacement inherited schema")
	}
	_ = old.Close()
	if err := call(ctx, remote, "renew_lease", renew, &result); err != nil {
		t.Fatal(err)
	}
	got, _ = up.service.GetSession(context.Background(), "http-activity")
	if got.Activity != nil && got.Activity.State == broker.ActivityWorking {
		t.Fatal("new connection inherited activity capability")
	}
	if !remote.verifyActivity(ctx) || !remote.activityAllowed() {
		t.Fatal("replacement schema not rechecked")
	}
}

func TestActivityFileBudgetExhaustionKeepsLease(t *testing.T) {
	up := newSwitchableUpstream(t)
	home := t.TempDir()
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "sessions", "4242.json"), []byte(strings.Repeat("x", (64<<10)+1)), 0600); err != nil {
		t.Fatal(err)
	}
	f := newDurableFixtureWithPolicy(t, up, t.TempDir(), false, func(o *durableOptions, _ *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) {
		o.activityFactory = func(c localactivity.Config) activityObserver {
			c.Home = home
			c.Host = "fixture-host"
			c.Platform = "fixture-platform"
			c.Format = localactivity.ClaudeSessionV1
			c.Probe = func(context.Context, int) (localactivity.Process, error) {
				return localactivity.Process{Key: "fixture-process", RecordStart: "fixture-start", Host: "fixture-host", Platform: "fixture-platform"}, nil
			}
			return localactivity.New(c)
		}
	})
	f.identity.CWD = cwd
	_, res := f.attach(t, "file-budget")
	if res.IsError {
		t.Fatal("attach failed")
	}
	waitFor(t, "file budget fallback", func() bool {
		got, err := up.service.GetSession(context.Background(), "file-budget")
		return err == nil && got.Activity != nil && got.Activity.Reason == "budget-exceeded" && got.AutomaticDeliveryReady
	})
	f.stop()
	key := clientstate.RegistrationKey("http://broker/mcp?project=brk", "claude", f.identity.SessionID)
	diagnostics, err := f.state.LoadDiagnostics(key)
	if err != nil || diagnostics.Summary.LastRenewSuccess.IsZero() || !hasDiagnostic(diagnostics, "activity-unavailable", "budget-exceeded") {
		t.Fatal("missing safe activity diagnostic or successful renewal")
	}
	raw, _ := json.Marshal(diagnostics)
	if strings.Contains(string(raw), home) || strings.Contains(string(raw), cwd) {
		t.Fatal("diagnostics exposed paths")
	}
}

func TestActivityProjectBindingsUseOnlyOwnUpstream(t *testing.T) {
	const a = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const b = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	auth := newDeviceAuthServer(t)
	up := newSwitchableUpstream(t)
	callsA, callsB, shared := &activityCapture{}, &activityCapture{}, &activityCapture{Upstream: unavailableUpstream{}}
	identities := map[string]broker.AttachRequest{a: {Harness: "codex", SessionID: a, PID: 7, CWD: t.TempDir()}, b: {Harness: "claude", SessionID: b, PID: 7, CWD: t.TempDir()}}
	f := newDeviceModeFixture(t, auth, up, t.TempDir(), identities, func(f *deviceModeFixture) {
		f.sharedUpstream = shared
		f.wrapUpstream = func(credential string, inner Upstream) Upstream {
			p := callsA
			if credential == "asb_s_b" {
				p = callsB
			}
			p.Upstream = inner
			return p
		}
		f.configureDurable = func(o *durableOptions) {
			o.activityFactory = func(config localactivity.Config) activityObserver {
				return observerFunc(func(_ context.Context, bound localactivity.Binding) localactivity.Observation {
					state, source := localactivity.Working, "codex-rollout"
					if bound.SessionID == b {
						state, source = localactivity.Idle, "claude-session-file"
					}
					if bound != config.Binding {
						return localactivity.Observation{State: localactivity.Unknown, Reason: "identity-mismatch", ObservedAt: time.Now().UTC()}
					}
					return localactivity.Observation{State: state, Source: source, ObservedAt: time.Now().UTC()}
				})
			}
		}
	})
	for _, id := range []string{a, b} {
		if f.status(id)["status"] != "pending" {
			t.Fatal("expected approval pending")
		}
	}
	if len(shared.snapshot()) != 0 || len(callsA.snapshot()) != 0 {
		t.Fatal("activity before authorization")
	}
	f.grant(a, "asb_s_a", "activity-a")
	f.grant(b, "asb_s_b", "activity-b")
	waitFor(t, "separate activity states", func() bool {
		sa, ea := up.service.GetSession(context.Background(), "activity-a")
		sb, eb := up.service.GetSession(context.Background(), "activity-b")
		return ea == nil && eb == nil && sa.Activity != nil && sb.Activity != nil && sa.Activity.State == broker.ActivityWorking && sb.Activity.State == broker.ActivityIdle
	})
	sa, _ := up.service.GetSession(context.Background(), "activity-a")
	sb, _ := up.service.GetSession(context.Background(), "activity-b")
	f.stop()
	for _, entry := range []struct {
		calls *activityCapture
		id    string
		state broker.ActivityState
	}{{callsA, sa.RegistrationID, broker.ActivityWorking}, {callsB, sb.RegistrationID, broker.ActivityIdle}} {
		found := false
		var last uint64
		for _, call := range entry.calls.snapshot() {
			if call.registration != entry.id {
				t.Fatal("activity crossed registrations")
			}
			if call.activity == nil {
				continue
			}
			if call.activity.Generation == 0 || call.activity.Sequence <= last {
				t.Fatal("invalid generation or sequence")
			}
			last = call.activity.Sequence
			if call.activity.State != broker.ActivityUnknown && call.activity.State != entry.state {
				t.Fatal("activity crossed conversations")
			}
			found = found || call.activity.State == entry.state
		}
		if !found {
			t.Fatal("missing bound observation")
		}
	}
	if len(shared.snapshot()) != 0 {
		t.Fatal("activity reached shared upstream")
	}
}

func TestActivityOldOrContradictoryServerOmitsArgument(t *testing.T) {
	for _, kind := range []string{"old-version", "schema-mismatch"} {
		t.Run(kind, func(t *testing.T) {
			up := newSwitchableUpstream(t)
			capture := &activityCapture{Upstream: up, oldServer: kind == "old-version", denySchema: kind == "schema-mismatch"}
			f := newDurableFixtureWithPolicy(t, up, t.TempDir(), false)
			// Replace the actual local server's upstream through the existing fixture
			// construction below; no generated tool list counts as a capability.
			f.stop()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			state := f.state
			opts := durableOptions{enabled: true, state: state, scope: "old-server", project: "brk", heartbeat: 10 * time.Millisecond, timeout: time.Second}.withDefaults()
			id := f.identity
			id.Name = "old-server"
			id.ChannelID = "old-lease"
			d, unlock, err := opts.loadRegistration(id)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			binding := &ingressBinding{mu: &sync.Mutex{}, identity: id, durable: d}
			registered, err := registerDurable(ctx, capture, binding, opts, id)
			if err != nil {
				t.Fatal(err)
			}
			binding.generation = registered.Generation
			done := make(chan receiverExit, 1)
			go func() {
				done <- receiveDurableOnce(ctx, capture, binding, opts, func(context.Context, broker.Message) error { return nil })
			}()
			waitFor(t, "old server heartbeat", func() bool { return len(capture.snapshot()) >= 3 })
			cancel()
			<-done
			for _, call := range capture.snapshot() {
				if call.activity != nil {
					t.Fatal("unsupported activity argument sent")
				}
			}
		})
	}
}

func TestActivitySchemaFailsClosed(t *testing.T) {
	for _, extra := range []string{"required", "allOf", "anyOf", "oneOf", "not", "if", "$ref"} {
		schema := map[string]any{"type": "object", "properties": map[string]any{"activity": map[string]any{}}}
		if extra == "required" {
			schema[extra] = []string{"activity"}
		} else {
			schema[extra] = map[string]any{}
		}
		if activitySchemaAllowed([]*mcp.Tool{{Name: "renew_lease", InputSchema: schema}}) {
			t.Fatal("ambiguous root schema accepted")
		}
	}
	for _, prop := range []any{false, nil, "bad", map[string]any{"type": "string"}, map[string]any{"$ref": "unknown"}, map[string]any{"type": "object", "required": []string{"secret"}}} {
		if activitySchemaAllowed([]*mcp.Tool{{Name: "renew_lease", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"activity": prop}}}}) {
			t.Fatal("contradictory schema accepted")
		}
	}
	if activitySchemaAllowed([]*mcp.Tool{{Name: "renew_lease", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}}}) {
		t.Fatal("missing activity accepted")
	}
}

func TestActivitySlowObserverDoesNotBlockRenewOrDelivery(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		t.Run(harness, func(t *testing.T) {
			up := newSwitchableUpstream(t)
			var samples atomic.Int32
			f := newDurableFixtureWithPolicy(t, up, t.TempDir(), false, func(o *durableOptions, _ *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) {
				o.activityFactory = func(localactivity.Config) activityObserver {
					return observerFunc(func(ctx context.Context, _ localactivity.Binding) localactivity.Observation {
						samples.Add(1)
						<-ctx.Done()
						return localactivity.Observation{State: localactivity.Working, Source: "secret-source", Reason: "secret-reason", ObservedAt: time.Now().UTC()}
					})
				}
			})
			f.identity.Harness = harness
			session, result := f.attach(t, "slow-observer")
			if result.IsError {
				t.Fatal("attach failed")
			}
			waitFor(t, "budget fallback with healthy renewal", func() bool {
				got, err := up.service.GetSession(context.Background(), "slow-observer")
				return err == nil && got.Activity != nil && got.Activity.Reason == "budget-exceeded" && got.AutomaticDeliveryReady && samples.Load() > 0
			})
			var sent broker.SendResult
			if err := call(context.Background(), up, "send_message", map[string]string{"from": "slow-observer", "to": "slow-observer", "body": "test delivery"}, &sent); err != nil {
				t.Fatal(err)
			}
			select {
			case m := <-f.sink:
				if m.RegistrationID != session.RegistrationID {
					t.Fatal("wrong receiver")
				}
			case <-time.After(time.Second):
				t.Fatal("observer blocked delivery")
			}
			f.stop()
		})
	}
}

func TestActivityWorkerBoundAndShutdownJoin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := newActivityWorkers(ctx)
	entered := make(chan struct{}, 2)
	var active, maxActive atomic.Int32
	// Simulate an uninterruptible file syscall: both slots stay occupied until
	// server cancellation, independently of the 100ms per-sample deadline.
	observer := observerFunc(func(_ context.Context, _ localactivity.Binding) localactivity.Observation {
		n := active.Add(1)
		for {
			old := maxActive.Load()
			if n <= old || maxActive.CompareAndSwap(old, n) {
				break
			}
		}
		entered <- struct{}{}
		<-ctx.Done()
		active.Add(-1)
		return localactivity.Observation{}
	})
	for range 2 {
		select {
		case p.jobs <- activityJob{ctx: ctx, observe: observer, result: make(chan localactivity.Observation, 1)}:
		case <-time.After(time.Second):
			t.Fatal("worker not started")
		}
		<-entered
	}
	select {
	case p.jobs <- activityJob{ctx: ctx, observe: observer}:
		t.Fatal("more than two simultaneous workers")
	default:
	}
	cancel()
	joined := make(chan struct{})
	go func() { p.wg.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not join observers")
	}
	if active.Load() != 0 || maxActive.Load() != 2 {
		t.Fatal("worker bound violated")
	}
}

func TestActivityLateResultAndNewLeaseSequence(t *testing.T) {
	up := newSwitchableUpstream(t)
	binding := &ingressBinding{mu: &sync.Mutex{}, identity: broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", ChannelID: "lease-a"}, durable: &durableRegistration{clientID: "client-a", registrationID: "reg-a"}, generation: 1, activityEnabled: true}
	opts := durableOptions{heartbeat: time.Second}
	first := opts.newActivityStream(binding)
	first.pending = make(chan localactivity.Observation, 1)
	first.pending <- localactivity.Observation{State: localactivity.Working, Source: "codex-rollout", ObservedAt: time.Now().UTC()}
	var one broker.Activity
	_ = json.Unmarshal(first.sample(context.Background(), binding, up), &one)
	if one.Sequence != 1 || one.State != broker.ActivityWorking {
		t.Fatal("first observation missing")
	}
	binding.mu.Lock()
	binding.identity.ChannelID = "lease-b"
	binding.generation = 2
	binding.mu.Unlock()
	first.pending = make(chan localactivity.Observation, 1)
	first.pending <- localactivity.Observation{State: localactivity.Working, Source: "codex-rollout", ObservedAt: time.Now().UTC()}
	if len(first.sample(context.Background(), binding, up)) != 0 {
		t.Fatal("late old-lease observation sent")
	}
	second := opts.newActivityStream(binding)
	var two broker.Activity
	_ = json.Unmarshal(second.sample(context.Background(), binding, up), &two)
	if two.Sequence != 1 || two.Generation != 2 || two.State != broker.ActivityUnknown {
		t.Fatal("new lease inherited state/sequence")
	}
	second.pending = make(chan localactivity.Observation, 1)
	second.pending <- localactivity.Observation{State: localactivity.Working, Source: "secret-source", Reason: "https://secret.invalid/?token=secret-marker", ObservedAt: time.Now().UTC()}
	raw := second.sample(context.Background(), binding, up)
	if strings.Contains(string(raw), "secret") || len(raw) > 2048 {
		t.Fatal("unsafe observation serialized")
	}
}

// Removing activity from renew must fail this test even when both receivers
// remain healthy: their observations belong to their own durable registration.
func TestActivityRenewBoundRegistration(t *testing.T) {
	u := newSwitchableUpstream(t)
	a := newDurableFixture(t, u, t.TempDir())
	b := newDurableFixture(t, u, t.TempDir())
	b.identity.SessionID = "22222222-2222-4222-8222-222222222222"
	sa, ra := a.attach(t, "activity-a")
	sb, rb := b.attach(t, "activity-b")
	if ra.IsError || rb.IsError {
		t.Fatal("attach failed")
	}
	waitFor(t, "A activity on registration A", func() bool {
		got, err := u.service.GetSession(context.Background(), "activity-a")
		return err == nil && got.RegistrationID == sa.RegistrationID && got.Activity != nil && got.Activity.Sequence > 0 && got.Activity.Reason == "unsupported"
	})
	waitFor(t, "B activity on registration B", func() bool {
		got, err := u.service.GetSession(context.Background(), "activity-b")
		return err == nil && got.RegistrationID == sb.RegistrationID && got.Activity != nil && got.Activity.Sequence > 0 && got.Activity.Reason == "unsupported"
	})
}

func TestActivityClientSelectsOnlyNarrowCodexFormat(t *testing.T) {
	for _, harness := range []string{"codex", "claude"} {
		t.Run(harness, func(t *testing.T) {
			binding := &ingressBinding{mu: &sync.Mutex{}, identity: broker.AttachRequest{Harness: harness, SessionID: "11111111-1111-4111-8111-111111111111", ChannelID: "lease", PID: 7, CWD: t.TempDir()}, durable: &durableRegistration{clientID: "client", registrationID: "reg"}, generation: 1}
			home := t.TempDir()
			cwd := binding.identity.CWD
			opts := durableOptions{activityFactory: func(c localactivity.Config) activityObserver {
				expected := ""
				if harness == "codex" {
					expected = localactivity.CodexTaskPairV1
				}
				if c.Format != expected {
					t.Fatalf("%s selected format %q, want %q", harness, c.Format, expected)
				}
				c.Home = home
				c.Host = "host"
				c.Platform = "test"
				c.Probe = func(context.Context, int) (localactivity.Process, error) {
					return localactivity.Process{Key: "created", Host: "host", Platform: "test"}, nil
				}
				return localactivity.New(c)
			}}
			stream := opts.newActivityStream(binding)
			if harness == "claude" {
				got := stream.observer.Observe(context.Background(), stream.binding)
				if got.State != localactivity.Unknown || got.Reason != "unsupported" {
					t.Fatalf("Claude enabled: %+v", got)
				}
				return
			}
			dir := filepath.Join(home, "sessions", "2026", "10", "10")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": binding.identity.SessionID, "cwd": cwd}})
			event, _ := json.Marshal(map[string]any{"type": "event_msg", "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "payload": map[string]any{"type": "task_started", "turn_id": "same-turn"}})
			path := filepath.Join(dir, "rollout-fixture-"+binding.identity.SessionID+".jsonl")
			if err := os.WriteFile(path, append(append(meta, '\n'), append(event, '\n')...), 0600); err != nil {
				t.Fatal(err)
			}
			got := stream.observer.Observe(context.Background(), stream.binding)
			if got.State != localactivity.Working {
				t.Fatalf("Codex schema not enabled: %+v", got)
			}
		})
	}
}
