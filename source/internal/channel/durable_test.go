package channel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpconfig"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
	"github.com/xelexis-dev/agent-session-broker/source/internal/memory"
)

type noMemory struct{}

func (noMemory) Put(context.Context, memory.PutInput) (memory.Node, error) {
	return memory.Node{}, errors.New("unused")
}
func (noMemory) Get(context.Context, string) (memory.GetResult, error) {
	return memory.GetResult{}, errors.New("unused")
}
func (noMemory) Search(context.Context, memory.SearchInput) (memory.SearchResult, error) {
	return memory.SearchResult{}, errors.New("unused")
}

// switchableUpstream fronts an in-process durable broker whose instance can be
// replaced to simulate a server restart. While offline every call fails the
// way a dropped HTTP connection does.
type switchableUpstream struct {
	t          *testing.T
	root       string
	mu         sync.Mutex
	service    *broker.RemoteService
	server     *mcp.ServerSession
	client     *mcp.ClientSession
	offline    atomic.Bool
	probeAfter time.Duration
	// dropNext names one tool whose next call is executed by the broker but
	// whose answer is lost on the way back, like a reset connection.
	dropNext atomic.Value
	// loseAtDeadline applies one request, then loses its answer on cancellation.
	loseAtDeadline atomic.Value
	// appliedBeforeAnswerLoss signals successful server apply before withholding a reply.
	appliedBeforeAnswerLoss chan struct{}
}

func (u *switchableUpstream) loseNextAnswer(tool string) { u.dropNext.Store(tool) }

func newSwitchableUpstream(t *testing.T) *switchableUpstream {
	t.Helper()
	u := &switchableUpstream{t: t, root: t.TempDir()}
	u.restart()
	t.Cleanup(func() {
		u.mu.Lock()
		defer u.mu.Unlock()
		if u.service != nil {
			_ = u.service.Close()
		}
		if u.server != nil {
			u.server.Close()
		}
		if u.client != nil {
			u.client.Close()
		}
	})
	return u
}

func (u *switchableUpstream) restart() {
	u.t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	// Closing the service first releases blocked receivers, as a real
	// shutdown does, so the session close does not wait for lease timers.
	if u.service != nil {
		_ = u.service.Close()
	}
	if u.server != nil {
		u.server.Close()
	}
	if u.client != nil {
		u.client.Close()
	}
	service, err := broker.NewPersistentRemoteService(u.root)
	if err != nil {
		u.t.Fatal(err)
	}
	if u.probeAfter > 0 {
		service.SetProbeSchedule(u.probeAfter, time.Minute, 0)
	}
	st, ct := mcp.NewInMemoryTransports()
	server, err := mcpserver.New(service, noMemory{}).Connect(context.Background(), st, nil)
	if err != nil {
		u.t.Fatal(err)
	}
	client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.17.3"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		u.t.Fatal(err)
	}
	u.service, u.server, u.client = service, server, client
}

func (u *switchableUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if u.offline.Load() {
		return nil, errors.New("connection refused")
	}
	u.mu.Lock()
	client := u.client
	u.mu.Unlock()
	// These legacy fixtures share one actual Go receiving process. Modernize
	// only absent metadata; explicit different/legacy holder tests use the real
	// service directly and are never rewritten by this fixture adapter.
	if p.Name == "register_session" || p.Name == "resume_session" || p.Name == "rebind_session" {
		raw, _ := json.Marshal(p.Arguments)
		var args map[string]any
		if json.Unmarshal(raw, &args) == nil && args["holder"] == nil {
			args["holder"] = fixtureHolder()
			copy := *p
			copy.Arguments = args
			p = &copy
		}
	}
	result, err := client.CallTool(ctx, p)
	if late, _ := u.loseAtDeadline.Load().(string); late != "" && late == p.Name {
		u.loseAtDeadline.Store("")
		if err == nil && result != nil && !result.IsError && u.appliedBeforeAnswerLoss != nil {
			close(u.appliedBeforeAnswerLoss)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if drop, _ := u.dropNext.Load().(string); drop != "" && drop == p.Name {
		u.dropNext.Store("")
		return nil, errors.New("connection reset by peer")
	}
	return result, err
}

func (u *switchableUpstream) tools() []*mcp.Tool {
	u.mu.Lock()
	client := u.client
	u.mu.Unlock()
	listed, err := client.ListTools(context.Background(), nil)
	if err != nil {
		u.t.Fatal(err)
	}
	return listed.Tools
}

func (u *switchableUpstream) session(name string) (broker.Session, error) {
	var got broker.Session
	err := call(context.Background(), u, "get_session", map[string]string{"name": name}, &got)
	return got, err
}

type durableFixture struct {
	upstream    *switchableUpstream
	state       *clientstate.Store
	identity    broker.AttachRequest
	observed    atomic.Pointer[broker.AttachRequest]
	observedFor atomic.Pointer[broker.AttachRequest]
	local       *mcp.ClientSession
	sink        chan broker.Message
	cancel      context.CancelFunc
	shutdown    func()
}

// stop ends the client like a process exit: cancel, then join receivers so
// no state write or lock outlives it.
func (f *durableFixture) stop() {
	f.cancel()
	f.shutdown()
}

func newDurableFixture(t *testing.T, upstream *switchableUpstream, stateDir string) *durableFixture {
	return newDurableFixtureWithPolicy(t, upstream, stateDir, false)
}

func newDurableFixtureWithPolicy(t *testing.T, upstream *switchableUpstream, stateDir string, tokenMode bool, configure ...func(*durableOptions, *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error))) *durableFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	state, err := clientstate.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	f := &durableFixture{upstream: upstream, state: state, sink: make(chan broker.Message, 8), cancel: cancel}
	f.identity = broker.AttachRequest{Harness: "claude", SessionID: "11111111-1111-4111-8111-111111111111", PID: 4242, CWD: `C:\work\brk`}
	f.observed.Store(&f.identity)
	opts := durableOptions{
		enabled: true, tokenMode: tokenMode, state: state, project: "brk", scope: "http://broker/mcp?project=brk",
		observe: func(_ context.Context, bound broker.AttachRequest) (broker.AttachRequest, error) {
			f.observedFor.Store(&bound)
			return *f.observed.Load(), nil
		},
		heartbeat: 50 * time.Millisecond, timeout: time.Second,
		backoff: func(int) time.Duration { return 20 * time.Millisecond },
	}
	identify := func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
		return f.identity, nil
	}
	for _, change := range configure {
		change(&opts, &identify)
	}
	server, shutdown := newIngressServerWithOptions(ctx, upstream, upstream.tools(), identify, func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
		return func(_ context.Context, m broker.Message) error { f.sink <- m; return nil }, nil, nil
	}, opts)
	f.shutdown = shutdown
	// LIFO cleanup: cancel runs first, then the join, then the TempDir removal.
	t.Cleanup(shutdown)
	t.Cleanup(cancel)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	f.local, err = mcp.NewClient(&mcp.Implementation{Name: "harness", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.local.Close() })
	return f
}

func (f *durableFixture) attach(t *testing.T, name string) (broker.Session, *mcp.CallToolResult) {
	t.Helper()
	result, err := f.local.CallTool(context.Background(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]string{"name": name}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		return broker.Session{}, result
	}
	data, _ := json.Marshal(result.StructuredContent)
	var session broker.Session
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatal(err)
	}
	return session, result
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestDurableClientRegistersKeepsSecretLocalAndResumesAfterRestart(t *testing.T) {
	upstream := newSwitchableUpstream(t)
	stateDir := t.TempDir()
	f := newDurableFixture(t, upstream, stateDir)
	session, result := f.attach(t, "owner")
	if result.IsError || session.RegistrationID == "" || !session.AutomaticDeliveryReady || session.Name != "owner" || session.Revision != 1 {
		t.Fatalf("attach: %+v %#v", session, result)
	}
	files, _ := filepath.Glob(filepath.Join(stateDir, "registrations", "*.json"))
	if len(files) != 1 {
		t.Fatalf("registration state files: %v", files)
	}
	raw, _ := os.ReadFile(files[0])
	var saved clientstate.Registration
	if json.Unmarshal(raw, &saved) != nil || saved.Secret == "" || saved.RegistrationID != session.RegistrationID || saved.Name != "owner" {
		t.Fatalf("saved state: %s", raw)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), saved.Secret) {
		t.Fatal("model-facing result exposes the resume secret")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "client-id")); err != nil {
		t.Fatal(err)
	}
	if err := call(context.Background(), upstream, "send_message", broker.Message{From: "owner", To: "owner", Body: "before restart"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-f.sink:
		if m.Body != "before restart" {
			t.Fatal(m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no delivery before restart")
	}
	// The model re-running register keeps the same registration and lease.
	again, result := f.attach(t, "owner")
	if result.IsError || again.RegistrationID != session.RegistrationID || again.Generation != session.Generation {
		t.Fatalf("re-register: %+v %#v", again, result)
	}

	upstream.restart()
	waitFor(t, "automatic resume after restart", func() bool {
		got, err := upstream.session("owner")
		return err == nil && got.Status == broker.StatusAlive && got.AutomaticDeliveryReady && got.Revision > again.Revision && got.Epoch != session.Epoch
	})
	if err := call(context.Background(), upstream, "send_message", broker.Message{From: "owner", To: "owner", Body: "after restart"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-f.sink:
		if m.Body != "after restart" {
			t.Fatal(m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no delivery after automatic resume")
	}
	raw, _ = os.ReadFile(files[0])
	_ = json.Unmarshal(raw, &saved)
	if got, _ := upstream.session("owner"); saved.Revision != got.Revision {
		t.Fatalf("local revision %d not updated to %d", saved.Revision, got.Revision)
	}

	// A network outage longer than several heartbeats: the client keeps
	// retrying until the broker is reachable again, without the model.
	upstream.offline.Store(true)
	time.Sleep(300 * time.Millisecond)
	upstream.restart()
	upstream.offline.Store(false)
	waitFor(t, "resume after outage", func() bool {
		got, err := upstream.session("owner")
		return err == nil && got.Status == broker.StatusAlive
	})
}

func TestDurableClientAnswersProbeWithoutModelAndStopsOnMismatch(t *testing.T) {
	upstream := newSwitchableUpstream(t)
	f := newDurableFixture(t, upstream, t.TempDir())
	session, result := f.attach(t, "probed")
	if result.IsError {
		t.Fatalf("attach: %#v", result)
	}
	// Make the restarted broker issue a probe immediately.
	upstream.probeAfter = time.Nanosecond
	upstream.restart()
	waitFor(t, "probe answered after resume", func() bool {
		got, err := upstream.session("probed")
		return err == nil && got.Status == broker.StatusAlive && !got.ProbePending && got.Revision > session.Revision
	})
	if bound := f.observedFor.Load(); bound == nil || bound.SessionID != f.identity.SessionID || bound.PID != f.identity.PID || bound.Harness != "claude" {
		t.Fatalf("probe re-observation did not receive the bound identity: %+v", bound)
	}

	// The parent observed by the client no longer matches the registration.
	changed := f.identity
	changed.PID = 9999
	f.observed.Store(&changed)
	upstream.restart()
	waitFor(t, "registration missing after identity mismatch", func() bool {
		got, err := upstream.session("probed")
		return err == nil && got.Status == broker.StatusMissing && got.ErrorCode == "identity-mismatch" && got.RegistrationID == session.RegistrationID
	})
	time.Sleep(150 * time.Millisecond)
	if got, err := upstream.session("probed"); err != nil || got.Status != broker.StatusMissing || got.AutomaticDeliveryReady {
		t.Fatal("client resumed a mismatched registration automatically", err)
	}
	// Explicit registration repairs the mismatch while retaining ID and name.
	f.observed.Store(&f.identity)
	fresh, result := f.attach(t, "probed")
	if result.IsError || fresh.RegistrationID != session.RegistrationID || fresh.Name != "probed" || fresh.Revision <= session.Revision {
		t.Fatalf("explicit re-register after mismatch: %+v %#v", fresh, result)
	}
}

func TestDurableClientDoesNotResumeAfterDetachAndAdoptsAssignedNickname(t *testing.T) {
	upstream := newSwitchableUpstream(t)
	var binding atomic.Pointer[ingressBinding]
	f := newDurableFixtureWithPolicy(t, upstream, t.TempDir(), false, func(o *durableOptions, _ *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) {
		o.bindingCreated = binding.Store
	})
	session, result := f.attach(t, "owner")
	if result.IsError {
		t.Fatalf("attach: %#v", result)
	}
	bound := binding.Load()
	bound.mu.Lock()
	receiverDone := bound.done
	bound.mu.Unlock()
	if err := call(context.Background(), upstream, "stop_session", map[string]string{"name": "owner"}, nil); err != nil {
		t.Fatal(err)
	}
	// Explicit re-registration may replace this binding only after its receiver exits.
	select {
	case <-receiverDone:
	case <-time.After(5 * time.Second):
		t.Fatal("receiver did not stop after detach")
	}
	got, err := upstream.session("owner")
	if err != nil || got.ErrorCode != "detached" || got.Revision != session.Revision {
		t.Fatalf("automatic resume revived a detached registration: %+v %v", got, err)
	}
	revived, result := f.attach(t, "owner")
	if result.IsError || revived.RegistrationID != session.RegistrationID || revived.Status != broker.StatusAlive {
		t.Fatalf("explicit register after detach: %+v %#v", revived, result)
	}

	// Another client already holds the requested name: the broker assigns a
	// UUID nickname and this client receives on it.
	other := broker.RegisterRequest{Harness: "codex", SessionID: "22222222-2222-4222-8222-222222222222", PID: 77, CWD: `C:\other`, ClientID: "client-other-000001", Name: "shared", ChannelID: "other-lease", Secret: strings.Repeat("o", 40), RequestID: "other-req"}
	var otherResult broker.RegisterResult
	if err := call(context.Background(), upstream, "register_session", other, &otherResult); err != nil || otherResult.Code != "" {
		t.Fatal(otherResult, err)
	}
	renamed, result := f.attach(t, "shared")
	if result.IsError || renamed.Name == "shared" || renamed.RequestedName != "shared" || renamed.RegistrationID != session.RegistrationID {
		t.Fatalf("nickname conflict handling: %+v %#v", renamed, result)
	}
	if err := call(context.Background(), upstream, "send_message", broker.Message{From: "shared", To: renamed.Name, Body: "to nickname"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-f.sink:
		if m.Body != "to nickname" || m.To != renamed.Name {
			t.Fatal(m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no delivery on the assigned nickname")
	}
}

func TestDurableClientSecondInstanceIsLockedOut(t *testing.T) {
	upstream := newSwitchableUpstream(t)
	stateDir := t.TempDir()
	first := newDurableFixture(t, upstream, stateDir)
	if _, result := first.attach(t, "owner"); result.IsError {
		t.Fatalf("first attach: %#v", result)
	}
	second := newDurableFixture(t, upstream, stateDir)
	_, result := second.attach(t, "owner")
	if !result.IsError {
		t.Fatal("second local client for the same registration was admitted")
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if text != wantLocalLockMessage {
		t.Fatalf("lock error not surfaced: %q", text)
	}
	if got, _ := upstream.session("owner"); got.Revision != 1 {
		t.Fatalf("locked-out client changed the registration: %+v", got)
	}
}

type grantAdmission struct{ name string }

func (g grantAdmission) Admit(context.Context, broker.RegisterRequest) (string, string, error) {
	return g.name, "", nil
}

func TestDurableClientTokenModeRegistersWithoutNameAndHidesInternalTools(t *testing.T) {
	upstream := newSwitchableUpstream(t)
	upstream.service.SetAdmission(grantAdmission{name: "ui-reserved"})
	f := newDurableFixtureWithPolicy(t, upstream, t.TempDir(), true)
	listed, err := f.local.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Name != "attach_session" && internalUpstreamTools[tool.Name] {
			t.Fatalf("internal tool %s exposed to the model", tool.Name)
		}
		if tool.Name == "attach_session" {
			schema, _ := json.Marshal(tool.InputSchema)
			if strings.Contains(string(schema), `"required":["name"]`) {
				t.Fatalf("token mode still requires a name: %s", schema)
			}
		}
	}
	result, err := f.local.CallTool(context.Background(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("nameless attach in token mode: %#v %v", result, err)
	}
	data, _ := json.Marshal(result.StructuredContent)
	var session broker.Session
	_ = json.Unmarshal(data, &session)
	if session.Name != "ui-reserved" || !session.AutomaticDeliveryReady {
		t.Fatalf("assigned name not adopted: %+v", session)
	}
	// A tampered requested name is ignored by the broker's grant.
	forged, _ := f.attach(t, "admin-console")
	if forged.Name != "ui-reserved" || forged.RegistrationID != session.RegistrationID {
		t.Fatalf("requested name overrode the grant: %+v", forged)
	}
	if err := call(context.Background(), upstream, "send_message", broker.Message{From: "ui-reserved", To: "ui-reserved", Body: "granted"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-f.sink:
		if m.To != "ui-reserved" {
			t.Fatal(m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no delivery on the granted name")
	}
	open := newDurableFixture(t, newSwitchableUpstream(t), t.TempDir())
	if _, result := open.attach(t, ""); !result.IsError {
		t.Fatal("open mode accepted a nameless registration")
	}
}

func TestDurableClientRecoversLostRegisterAndResumeAnswers(t *testing.T) {
	upstream := newSwitchableUpstream(t)
	stateDir := t.TempDir()
	f := newDurableFixture(t, upstream, stateDir)
	upstream.loseNextAnswer("register_session")
	if _, result := f.attach(t, "owner"); !result.IsError {
		t.Fatal("lost register answer reported as success")
	}
	files, _ := filepath.Glob(filepath.Join(stateDir, "registrations", "*.json"))
	if len(files) != 1 {
		t.Fatalf("pending state not saved: %v", files)
	}
	raw, _ := os.ReadFile(files[0])
	var pending clientstate.Registration
	if json.Unmarshal(raw, &pending) != nil || pending.PendingRequestID == "" || pending.PendingLease == "" || pending.Secret == "" || pending.RegistrationID != "" {
		t.Fatalf("pending state: %s", raw)
	}
	// The client process restarts before retrying: the new process reads the
	// pending secret, request id and lease and replays the same request, so
	// the broker neither creates a second registration nor fences the first.
	f.stop()
	f = newDurableFixture(t, upstream, stateDir)
	session, result := f.attach(t, "owner")
	if result.IsError || session.RegistrationID == "" || session.Revision != 1 {
		t.Fatalf("retry from a new process after lost answer: %+v %#v", session, result)
	}
	var listed []broker.Session
	if err := call(context.Background(), upstream, "list_sessions", struct{}{}, &listed); err != nil || len(listed) != 1 {
		t.Fatalf("registrations after retry: %+v %v", listed, err)
	}
	raw, _ = os.ReadFile(files[0])
	var saved clientstate.Registration
	_ = json.Unmarshal(raw, &saved)
	if saved.PendingRequestID != "" || saved.RegistrationID != session.RegistrationID || saved.Secret != pending.Secret {
		t.Fatalf("state after success: %s", raw)
	}
	if err := call(context.Background(), upstream, "send_message", broker.Message{From: "owner", To: "owner", Body: "registered once"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.sink:
	case <-time.After(3 * time.Second):
		t.Fatal("no delivery after replayed registration")
	}

	// A resume the broker applied but whose answer was lost is retried as the
	// same request, so the lease the broker issued is the one the client uses.
	upstream.loseNextAnswer("resume_session")
	upstream.restart()
	waitFor(t, "resume after lost answer", func() bool {
		got, err := upstream.session("owner")
		return err == nil && got.Status == broker.StatusAlive && got.AutomaticDeliveryReady
	})
	got, _ := upstream.session("owner")
	if got.Revision != session.Revision+1 {
		t.Fatalf("lost resume answer caused extra lease replacements: %+v", got)
	}
	if err := call(context.Background(), upstream, "send_message", broker.Message{From: "owner", To: "owner", Body: "resumed once"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-f.sink:
		if m.Body != "resumed once" {
			t.Fatal(m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no delivery after replayed resume")
	}
}

func TestDurableChannelModeRegistersAndPushesEventsAcrossRestart(t *testing.T) {
	testDurableChannelLifecycle(t, false)
}

func TestDurableChannelDiscoveryRegistersAndPushesEventsAcrossRestart(t *testing.T) {
	testDurableChannelLifecycle(t, true)
}

func testDurableChannelLifecycle(t *testing.T, discover bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	upstream := newSwitchableUpstream(t)
	upstream.service.SetAdmission(grantAdmission{name: "channel-owner"})
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	identity := broker.AttachRequest{Harness: "claude", SessionID: "33333333-3333-4333-8333-333333333333", PID: 515, CWD: `C:\work\chan`}
	opts := durableOptions{
		enabled: true, tokenMode: true, state: state, project: "brk", scope: "http://broker/mcp?project=brk",
		observe:   func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return identity, nil },
		heartbeat: 50 * time.Millisecond, timeout: time.Second,
		backoff: func(int) time.Duration { return 20 * time.Millisecond },
	}
	st, ct := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- ServeDurableChannel(ctx, st, upstream, upstream.tools(), identity, opts) }()
	conn, err := ct.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	send := func(text string) {
		t.Helper()
		m, err := jsonrpc.DecodeMessage([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Write(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	read := func() []byte {
		t.Helper()
		m, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := jsonrpc.EncodeMessage(m)
		return b
	}
	if discover {
		// A rejected discovery must not start registration or channel activation.
		send(`{"jsonrpc":"2.0","id":0,"method":"server/discover","params":{"_meta":false}}`)
		var rejected struct {
			Error *json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(read(), &rejected); err != nil || rejected.Error == nil {
			t.Fatalf("invalid discovery was not rejected: %+v %v", rejected, err)
		}
		// Wait beyond the channel activation timer. A spurious event would be
		// the next frame instead of the valid discovery result read below.
		time.Sleep(600 * time.Millisecond)
		if _, err := upstream.session("channel-owner"); err == nil {
			t.Fatal("invalid discovery started automatic registration")
		}
		send(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"test","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}}}`)
	} else {
		send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	}
	var init struct {
		Result struct {
			Capabilities struct {
				Experimental map[string]any `json:"experimental"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	initialResult := read()
	if err := json.Unmarshal(initialResult, &init); err != nil {
		t.Fatal(err)
	}
	if _, ok := init.Result.Capabilities.Experimental["claude/channel"]; !ok {
		t.Fatalf("missing channel capability: %s", initialResult)
	}
	if !discover {
		send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	}
	send(`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"attach_session","arguments":{}}}`)
	_ = read()
	waitFor(t, "explicit legacy registration", func() bool { _, err := upstream.session("channel-owner"); return err == nil })
	pending, _ := upstream.session("channel-owner")
	if pending.AutomaticDeliveryReady {
		t.Fatal("legacy channel advertised ready before activation")
	}
	var event struct {
		Method string `json:"method"`
		Params struct {
			Meta    map[string]string `json:"meta"`
			Content string            `json:"content"`
		} `json:"params"`
	}
	if err := json.Unmarshal(read(), &event); err != nil || event.Params.Meta["activation"] == "" {
		t.Fatalf("activation event: %+v %v", event, err)
	}
	activate, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "activate_channel", "arguments": map[string]string{"token": event.Params.Meta["activation"]}}})
	send(string(activate))
	_ = read()
	waitFor(t, "channel ready after activation", func() bool {
		s, err := upstream.session("channel-owner")
		return err == nil && s.AutomaticDeliveryReady
	})
	expectEvent := func(body string) {
		t.Helper()
		var got struct {
			Method string `json:"method"`
			Params struct {
				Meta    map[string]string `json:"meta"`
				Content string            `json:"content"`
			} `json:"params"`
		}
		raw := read()
		if err := json.Unmarshal(raw, &got); err != nil || got.Method != "notifications/claude/channel" || got.Params.Content != body || got.Params.Meta["to"] != "channel-owner" {
			t.Fatalf("channel event: %+v %v", got, err)
		}
		if _, err := time.Parse(time.RFC3339Nano, got.Params.Meta["sentAt"]); err != nil {
			t.Fatal("durable channel omitted sentAt")
		}
	}
	if err := call(ctx, upstream, "send_message", broker.Message{From: "channel-owner", To: "channel-owner", Body: "channel-nonce"}, nil); err != nil {
		t.Fatal(err)
	}
	expectEvent("channel-nonce")
	upstream.restart()
	waitFor(t, "channel client resumed after restart", func() bool {
		got, err := upstream.session("channel-owner")
		return err == nil && got.Status == broker.StatusAlive && got.Revision > 1 && got.AutomaticDeliveryReady
	})
	if err := call(ctx, upstream, "send_message", broker.Message{From: "channel-owner", To: "channel-owner", Body: "after-restart"}, nil); err != nil {
		t.Fatal(err)
	}
	expectEvent("after-restart")
	reply, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": map[string]any{"name": "reply", "arguments": map[string]string{"to": "channel-owner", "body": "self-reply"}}})
	send(string(reply))
	// The pushed event and the tool result race on the wire; accept either order.
	var sawResult, sawEvent bool
	for i := 0; i < 2; i++ {
		data := read()
		var frame struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Result struct {
				IsError bool `json:"isError"`
			} `json:"result"`
			Params struct {
				Content string `json:"content"`
			} `json:"params"`
		}
		if err := json.Unmarshal(data, &frame); err != nil {
			t.Fatal(err)
		}
		switch {
		case frame.Method == "notifications/claude/channel" && frame.Params.Content == "self-reply":
			sawEvent = true
		case frame.ID != nil && !frame.Result.IsError:
			sawResult = true
		default:
			t.Fatalf("unexpected frame after reply: %s", data)
		}
	}
	if !sawResult || !sawEvent {
		t.Fatalf("reply result=%v event=%v", sawResult, sawEvent)
	}
	cancel()
	conn.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("channel client did not stop")
	}
}

func TestDurableClientRefusesNewRegistrationsWhileShuttingDown(t *testing.T) {
	upstream := newSwitchableUpstream(t)
	stateDir := t.TempDir()
	f := newDurableFixture(t, upstream, stateDir)
	if _, result := f.attach(t, "owner"); result.IsError {
		t.Fatalf("attach: %#v", result)
	}
	f.stop()
	if _, result := f.attach(t, "owner"); !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "shutting down") {
		t.Fatalf("registration accepted after shutdown: %#v", result)
	}
	// The lock is released, so a replacement client on the same PC resumes.
	replacement := newDurableFixture(t, upstream, stateDir)
	session, result := replacement.attach(t, "owner")
	if result.IsError || session.Status != broker.StatusAlive {
		t.Fatalf("replacement client could not take over: %+v %#v", session, result)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(stateDir, "registrations", "*.tmp*")); len(leftovers) != 0 {
		t.Fatalf("state writes outlived shutdown: %v", leftovers)
	}
}

func TestBrokerUpstreamSendsBearerTokenOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var seen atomic.Value
	service := broker.NewRemoteService()
	handler := mcpserver.Handler(mcpserver.New(service, noMemory{}))
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("Authorization"))
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	up := newTestBrokerUpstream(ctx, proxy.URL+"/mcp?project=brk")
	t.Cleanup(func() { up.Close() })
	up.setBearerToken("admission-token")
	var sessions []broker.Session
	if err := call(ctx, up, "list_sessions", struct{}{}, &sessions); err != nil {
		t.Fatal(err)
	}
	if got, _ := seen.Load().(string); got != "Bearer admission-token" {
		t.Fatalf("authorization header = %q", got)
	}
	if scope := brokerScope("http://Broker:8787/mcp?project=brk&token=secret", "brk"); strings.Contains(scope, "secret") || scope != "http://broker:8787/mcp?project=brk" {
		t.Fatalf("scope = %q", scope)
	}
}

// Integration fixtures use a released client line rather than the unversioned
// development binary, which a release server deliberately refuses.
func newTestBrokerUpstream(ctx context.Context, endpoint string) *brokerUpstream {
	if mcpconfig.ContextVersion(ctx) == "dev" {
		ctx = mcpconfig.WithVersion(ctx, "1.0.0")
	}
	return newBrokerUpstream(ctx, endpoint)
}
