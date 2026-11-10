package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
)

type scopeAdmission struct{}

func (scopeAdmission) Reserved(context.Context) bool { return true }
func (scopeAdmission) Admit(context.Context, broker.RegisterRequest) (string, string, error) {
	return "restricted", "", nil
}

type scopeCalls struct {
	mu    sync.Mutex
	calls map[string]int
}

func (s *scopeCalls) count(name string) int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls[name] }

func scopeCentral(t *testing.T, scopes []string) (*brokerUpstream, *broker.RemoteService, *scopeCalls) {
	t.Helper()
	remote := broker.NewRemoteService()
	remote.SetAdmission(scopeAdmission{})
	t.Cleanup(func() { _ = remote.Close() })
	counts := &scopeCalls{calls: map[string]int{}}
	handler := mcpserver.Handler(mcpserver.New(remote, noMemory{}))
	central := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			data, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(data))
			var rpc struct {
				Method string
				Params struct{ Name string }
			}
			if json.Unmarshal(data, &rpc) == nil && rpc.Method == "tools/call" {
				counts.mu.Lock()
				counts.calls[rpc.Params.Name]++
				counts.mu.Unlock()
			}
		}
		// This fixture exercises the 0.17.1 sticky-denial compatibility path.
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r.WithContext(access.WithGrant(r.Context(), access.Grant{Scopes: scopes})))
		for k, v := range response.Header() {
			w.Header()[k] = v
		}
		var payload any
		body := response.Body.Bytes()
		if json.Unmarshal(body, &payload) == nil {
			stripScopeSignals(payload)
			body, _ = json.Marshal(payload)
			w.Header().Del("Content-Length")
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(body)
	}))
	t.Cleanup(central.Close)
	u := newTestBrokerUpstream(t.Context(), central.URL+"/mcp")
	u.setBearerToken("synthetic-session-credential")
	t.Cleanup(func() { _ = u.Close() })
	return u, remote, counts
}

func TestRestrictedCredentialReceivesOnceThenKeepsRenewing(t *testing.T) {
	u, remote, counts := scopeCentral(t, []string{access.SessionRegister})
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	identity := broker.AttachRequest{Name: "restricted", Harness: "codex", SessionID: "11111111-1111-4111-8111-111111112201", PID: 2201, CWD: t.TempDir()}
	session, err := u.session(ctx)
	if err != nil {
		t.Fatal("synthetic handshake failed")
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	opts := durableOptions{receiverIdentity: fixtureReceiverIdentity{}, enabled: true, tokenMode: true, state: state, project: "restricted", scope: "restricted", heartbeat: 10 * time.Millisecond, timeout: time.Second, backoff: func(int) time.Duration { return time.Millisecond }, observe: func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return identity, nil }}
	bindings := make(chan *ingressBinding, 1)
	opts.bindingCreated = func(b *ingressBinding) { bindings <- b }
	remote.SetProbeSchedule(time.Millisecond, time.Minute, 0)
	local, shutdown := newIngressServerWithOptions(ctx, u, tools.Tools, func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) { return identity, nil }, func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
		return func(context.Context, broker.Message) error { return nil }, nil, nil
	}, opts)
	defer shutdown()
	defer cancel()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := local.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	harness, err := mcp.NewClient(&mcp.Implementation{Name: "restricted", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer harness.Close()
	if result, err := harness.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}}); err != nil || result.IsError {
		t.Fatal("explicit registration failed")
	}
	waitFor(t, "first restricted receive", func() bool { return counts.count("wait_for_message") > 0 })
	// A watchdog bounds the test, and successful renewals are the causal barrier.
	// The previous code repeatedly resumes and receives instead of keeping renewal alive.
	deadline := time.Now().Add(3 * time.Second)
	for counts.count("renew_lease") < 5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := counts.count("wait_for_message"); got != 1 {
		t.Errorf("denied receive attempts=%d; want exactly one", got)
	}
	if got := counts.count("renew_lease"); got < 5 {
		t.Errorf("successful renewal requests=%d; want stable renewal after denial", got)
	}
	if got := counts.count("resume_session"); got != 1 {
		t.Errorf("pending ingress must use exactly one lease refresh, got %d", got)
	}
	if got := counts.count("report_ingress_failure"); got != 0 {
		t.Errorf("scope denial caused %d ingress failures", got)
	}
	sessions, _ := remote.ListSessions(t.Context())
	if len(sessions) != 1 || sessions[0].AutomaticDeliveryReady {
		t.Error("restricted registration must stay registered with ingress pending")
	}
	for _, tc := range []struct {
		name string
		args any
	}{
		{"wait_for_message", map[string]string{"recipient": "restricted"}},
		{"ingress_hook", map[string]string{"session_id": identity.SessionID, "hook_event_name": "PostToolUse"}},
	} {
		result, err := harness.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
		if err != nil || !result.IsError || len(result.Content) != 1 || result.Content[0].(*mcp.TextContent).Text != errReceiveUnavailable.Error() {
			t.Errorf("%s must report fixed receive unavailable", tc.name)
		}
	}
	// Observe a bounded window after the barrier; further waits must not restart.
	time.Sleep(100 * time.Millisecond)
	if got := counts.count("wait_for_message"); got != 1 {
		t.Errorf("receive restarted after renew: %d", got)
	}
	b := <-bindings
	b.mu.Lock()
	registrationID, lease := b.durable.registrationID, b.identity.ChannelID
	b.mu.Unlock()
	// Replace only the HTTP MCP connection, then force a fresh ingress lease.
	// Neither event may erase this credential's process-local denial.
	u.mu.Lock()
	old := u.current
	u.current = nil
	u.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	before := counts.count("renew_lease")
	if err := remote.ReportIngressFailure(t.Context(), registrationID, lease); err != nil {
		t.Fatal("fixture lease retirement failed")
	}
	waitFor(t, "renewal after reconnect and lease change", func() bool { return counts.count("resume_session") >= 2 && counts.count("renew_lease") >= before+5 })
	if counts.count("wait_for_message") != 1 || counts.count("probe_response") == 0 {
		t.Fatal("denial memory or probe renewal lost across recovery")
	}
}

// Permission decisions are learned from the fixed server result, not token
// syntax, error substrings or a client-provided scope list.
type scopeReplyUpstream struct {
	mu     sync.Mutex
	calls  int
	result *mcp.CallToolResult
}

func (u *scopeReplyUpstream) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	return u.result, nil
}
func (u *scopeReplyUpstream) count() int { u.mu.Lock(); defer u.mu.Unlock(); return u.calls }

func TestReceiveDenialMemoryAcrossConnectionsAndCredentials(t *testing.T) {
	opts := (durableOptions{}).withDefaults()
	denied := &scopeReplyUpstream{result: &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "insufficient_scope"}}}}
	first := opts.wrapReceive(denied, "synthetic-credential-a")
	params := &mcp.CallToolParams{Name: "wait_for_message", Arguments: map[string]string{"recipient": "caller"}}
	var callers sync.WaitGroup
	for i := 0; i < 12; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			res, err := first.CallTool(t.Context(), params)
			if err != nil || !insufficientScope(res) {
				t.Error("fixed scope result lost")
			}
		}()
	}
	callers.Wait()
	if denied.count() != 1 {
		t.Fatal("concurrent denials made more than one network receive attempt")
	}
	replacement := &scopeReplyUpstream{result: denied.result}
	same := opts.wrapReceive(replacement, "synthetic-credential-a")
	if res, e := same.CallTool(t.Context(), params); e != nil || !insufficientScope(res) || replacement.count() != 0 {
		t.Fatal("connection replacement forgot credential denial")
	}
	full := &scopeReplyUpstream{result: &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "new credential result"}}}}
	fresh := opts.wrapReceive(full, "synthetic-credential-b")
	if _, e := fresh.CallTool(t.Context(), params); e != nil || full.count() != 1 || receiveDenied(fresh) {
		t.Fatal("new credential inherited denial")
	}
	// Re-selecting an old credential still cannot erase its process-lifetime denial.
	if _, e := opts.wrapReceive(replacement, "synthetic-credential-a").CallTool(t.Context(), params); e != nil || replacement.count() != 0 {
		t.Fatal("old credential denial was erased")
	}
}

func TestInternalScopeSignalIsFixedAndNotCredentialRejection(t *testing.T) {
	for _, text := range []string{"insufficient_scope", "server diagnostic: insufficient_scope", "insufficient_scope extra"} {
		u := &scopeReplyUpstream{result: &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}}
		e := call(t.Context(), u, "memory_put", struct{}{}, nil)
		if text == "insufficient_scope" {
			if e != errInsufficientScope || upstreamRejected(u) {
				t.Fatal("scope denial lost or treated as credential rejection")
			}
		} else if e == errInsufficientScope {
			t.Fatal("untrusted error substring became a scope decision")
		}
	}
}

func TestReadDeniedResultsForwardUnchangedWithoutReconnect(t *testing.T) {
	u, remote, counts := scopeCentral(t, []string{access.MessageReceive, access.MessageSend, access.SessionRegister})
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	identity := broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111112202", PID: 2202, CWD: t.TempDir()}
	upstreamSession, err := u.session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := upstreamSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	delivered := make(chan struct{}, 1)
	opts := durableOptions{receiverIdentity: fixtureReceiverIdentity{}, enabled: true, tokenMode: true, state: state, heartbeat: 20 * time.Millisecond, timeout: time.Second, observe: func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return identity, nil }}
	local, shutdown := newIngressServerWithOptions(ctx, u, tools.Tools, func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) { return identity, nil }, func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
		return func(context.Context, broker.Message) error {
			select {
			case delivered <- struct{}{}:
			default:
			}
			return nil
		}, nil, nil
	}, opts)
	defer shutdown()
	defer cancel()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := local.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	harness, err := mcp.NewClient(&mcp.Implementation{Name: "readless", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer harness.Close()
	if result, err := harness.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}}); err != nil || result.IsError {
		t.Fatal("explicit registration failed")
	}
	waitFor(t, "readless automatic registration", func() bool {
		got, _ := remote.ListSessions(t.Context())
		return len(got) == 1 && got[0].AutomaticDeliveryReady
	})
	for _, name := range []string{"list_sessions", "get_session"} {
		args := map[string]string{}
		if name == "get_session" {
			args["name"] = "restricted"
		}
		result, err := harness.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || !insufficientScope(result) {
			t.Fatalf("%s did not preserve fixed server result", name)
		}
		if counts.count(name) != 1 {
			t.Fatalf("%s was retried", name)
		}
	}
	sent, err := harness.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]string{"from": "restricted", "to": "restricted", "body": "readless-send"}})
	if err != nil || sent.IsError {
		t.Fatal("message:send incorrectly requires session:read")
	}
	select {
	case <-delivered:
	case <-time.After(3 * time.Second):
		t.Fatal("readless send not delivered")
	}
	if counts.count("register_session") != 1 || counts.count("resume_session") != 0 || upstreamRejected(u) {
		t.Fatal("read denial caused registration/auth recovery")
	}
}

func TestRestrictedChannelActivationReportsUnavailable(t *testing.T) {
	u, remote, counts := scopeCentral(t, []string{access.SessionRegister})
	state, e := clientstate.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, e := u.session(ctx)
	if e != nil {
		t.Fatal(e)
	}
	tools, e := session.ListTools(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	identity := broker.AttachRequest{Harness: "claude", SessionID: "11111111-1111-4111-8111-111111112203", PID: 2203, CWD: t.TempDir()}
	opts := durableOptions{receiverIdentity: fixtureReceiverIdentity{}, enabled: true, tokenMode: true, state: state, heartbeat: 20 * time.Millisecond, timeout: time.Second, observe: func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return identity, nil }}
	st, ct := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- ServeDurableChannel(ctx, st, u, tools.Tools, identity, opts) }()
	conn, e := ct.Connect(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("restricted channel did not join")
		}
	}()
	send := func(v any) {
		t.Helper()
		raw, _ := json.Marshal(v)
		m, e := jsonrpc.DecodeMessage(raw)
		if e != nil {
			t.Fatal(e)
		}
		if e = conn.Write(ctx, m); e != nil {
			t.Fatal(e)
		}
	}
	read := func() []byte {
		t.Helper()
		m, e := conn.Read(ctx)
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := jsonrpc.EncodeMessage(m)
		return raw
	}
	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "restricted-channel", "version": "test"}}})
	_ = read()
	send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	var event struct {
		Params struct{ Meta map[string]string }
	}
	if json.Unmarshal(read(), &event) != nil || event.Params.Meta["activation"] == "" {
		t.Fatal("missing synthetic activation event")
	}
	activation := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "activate_channel", "arguments": map[string]string{"token": event.Params.Meta["activation"]}}}
	send(activation)
	_ = read()
	send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": "attach_session", "arguments": map[string]string{}}})
	_ = read()
	waitFor(t, "channel denial and stable renewal", func() bool {
		got, _ := remote.ListSessions(t.Context())
		return counts.count("wait_for_message") == 1 && counts.count("renew_lease") >= 5 && len(got) == 1 && got[0].Revision >= 2 && !got[0].AutomaticDeliveryReady
	})
	send(activation)
	var response struct {
		Result struct {
			IsError bool
			Content []struct{ Text string }
		}
	}
	if json.Unmarshal(read(), &response) != nil || !response.Result.IsError || len(response.Result.Content) != 1 || response.Result.Content[0].Text != errReceiveUnavailable.Error() {
		t.Fatal("activation did not report fixed receive unavailable")
	}
	if counts.count("wait_for_message") != 1 {
		t.Fatal("activation restarted restricted reception")
	}
}

func TestLegacyLongPollDoesNotBlockConcurrentManualWait(t *testing.T) {
	u, _, counts := scopeCentral(t, access.LegacyV1Scopes())
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	session, err := u.session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	identity := broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111112204", PID: 2204, CWD: t.TempDir()}
	bindings := make(chan *ingressBinding, 1)
	opts := durableOptions{receiverIdentity: fixtureReceiverIdentity{}, enabled: true, tokenMode: true, state: state, heartbeat: 20 * time.Millisecond, timeout: time.Second, observe: func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return identity, nil }, bindingCreated: func(b *ingressBinding) { bindings <- b }}
	local, shutdown := newIngressServerWithOptions(ctx, u, tools.Tools, func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) { return identity, nil }, func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
		return func(context.Context, broker.Message) error { return nil }, nil, nil
	}, opts)
	defer shutdown()
	defer cancel()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := local.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	harness, err := mcp.NewClient(&mcp.Implementation{Name: "legacy-concurrency", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer harness.Close()
	if result, err := harness.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}}); err != nil || result.IsError {
		t.Fatal("explicit registration failed")
	}
	waitFor(t, "blocked durable long poll", func() bool { return counts.count("wait_for_message") == 1 })
	b := <-bindings
	b.mu.Lock()
	lease := b.identity.ChannelID
	b.mu.Unlock()
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		_, _ = harness.CallTool(ctx, &mcp.CallToolParams{Name: "wait_for_message", Arguments: map[string]string{"recipient": "restricted", "channelId": lease}})
	}()
	defer func() {
		cancel()
		select {
		case <-joined:
		case <-time.After(3 * time.Second):
			t.Error("manual wait did not cancel and join")
		}
	}()
	deadline := time.Now().Add(3 * time.Second) // 2s decision bound plus a watchdog margin.
	for counts.count("wait_for_message") < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if counts.count("wait_for_message") != 2 {
		t.Error("manual wait did not reach upstream within the bound while the durable long poll remained blocked")
	}
	if counts.count("resume_session") != 0 {
		t.Error("legacy permission observation caused a reconnect")
	}
}

func TestReceiveRealDenialOverridesAllowedDecision(t *testing.T) {
	opts := (durableOptions{}).withDefaults()
	inner := &scopeReplyUpstream{result: &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "allowed result"}}}}
	u := opts.wrapReceive(inner, "synthetic-allowed-credential")
	params := &mcp.CallToolParams{Name: "wait_for_message"}
	if _, e := u.CallTool(t.Context(), params); e != nil {
		t.Fatal(e)
	}
	// A real denial overrides even an earlier successful permission observation.
	inner.mu.Lock()
	inner.result = scopeDeniedResult()
	inner.mu.Unlock()
	for i := 0; i < 2; i++ {
		if _, e := u.CallTool(t.Context(), params); e != nil {
			t.Fatal(e)
		}
	}
	if !receiveDenied(u) || inner.count() != 2 {
		t.Fatal("real denial did not suppress later network receives")
	}
}

type decidedLongPoll struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
}

func (u *decidedLongPoll) CallTool(ctx context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	u.mu.Lock()
	u.calls++
	first := u.calls == 1
	u.mu.Unlock()
	if first {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "known full permission"}}}, nil
	}
	u.entered <- struct{}{}
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestReceiveAllowedResultUngatesImmediately(t *testing.T) {
	inner := &decidedLongPoll{entered: make(chan struct{}, 2)}
	u := (durableOptions{}).withDefaults().wrapReceive(inner, "synthetic-full-credential")
	p := &mcp.CallToolParams{Name: "wait_for_message"}
	if _, e := u.CallTool(t.Context(), p); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var joined sync.WaitGroup
	for i := 0; i < 2; i++ {
		joined.Add(1)
		go func() { defer joined.Done(); _, _ = u.CallTool(ctx, p) }()
	}
	defer func() { cancel(); joined.Wait() }()
	watchdog := time.NewTimer(time.Second)
	defer watchdog.Stop()
	for i := 0; i < 2; i++ {
		select {
		case <-inner.entered:
		case <-watchdog.C:
			t.Fatal("a decided allowed credential still gated concurrent long polls")
		}
	}
}

func stripScopeSignals(value any) {
	switch v := value.(type) {
	case map[string]any:
		delete(v, "credentialScopes")
		delete(v, "credential-scopes-v1")
		for _, child := range v {
			stripScopeSignals(child)
		}
	case []any:
		for _, child := range v {
			stripScopeSignals(child)
		}
	}
}
