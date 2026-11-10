//go:build darwin || linux

package channel

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type kimiContractUpstream struct {
	mu        sync.Mutex
	session   broker.Session
	registers int
	messages  chan broker.Message
}

func (u *kimiContractUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch p.Name {
	case "register_session":
		var req broker.RegisterRequest
		raw, _ := json.Marshal(p.Arguments)
		_ = json.Unmarshal(raw, &req)
		u.registers++
		u.session = broker.Session{ID: req.SessionID, Harness: req.Harness, PID: req.PID, Name: req.Name, ClientID: req.ClientID, RegistrationID: strings.Repeat("A", 26), Revision: 1, Generation: 1, Status: broker.StatusAlive, AutomaticDeliveryReady: false}
		return &mcp.CallToolResult{StructuredContent: broker.RegisterResult{Session: u.session}}, nil
	case "renew_lease":
		return &mcp.CallToolResult{StructuredContent: broker.RegisterResult{Session: u.session}}, nil
	case "get_session":
		return &mcp.CallToolResult{StructuredContent: u.session}, nil
	case "wait_for_message":
		u.mu.Unlock()
		defer u.mu.Lock()
		select {
		case m := <-u.messages:
			return &mcp.CallToolResult{StructuredContent: m}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case "session_token_info":
		return &mcp.CallToolResult{IsError: true}, nil
	default:
		return &mcp.CallToolResult{}, nil
	}
}
func TestKimiRegistrationFalseReadinessAndPendingStatus(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cwd := t.TempDir()
	k := newKimiClient(os.Getpid(), cwd)
	k.key, _ = kimiProcessKey(os.Getpid())
	_, e := k.accept(kimiHookInput{Event: "UserPromptSubmit", SessionID: kimiTestSession, CWD: cwd})
	if e != nil {
		t.Fatal(e)
	}
	state, e := clientstate.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	up := &kimiContractUpstream{messages: make(chan broker.Message, 1)}
	opts := durableOptions{enabled: true, tokenMode: true, state: state, kimi: k, harness: "kimi", scope: "test-kimi", project: "test", heartbeat: 20 * time.Millisecond, observe: func(c context.Context, _ broker.AttachRequest) (broker.AttachRequest, error) {
		return k.identify(c, nil)
	}}
	var bind *ingressBinding
	opts.bindingCreated = func(b *ingressBinding) { bind = b }
	server, shutdown := newIngressServerWithOptions(ctx, up, snapshotTools(), k.identify, func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
		return k.queue.deliver, nil, nil
	}, opts)
	a, b := mcp.NewInMemoryTransports()
	ss, e := server.Connect(ctx, a, nil)
	if e != nil {
		t.Fatal(e)
	}
	cli, e := mcp.NewClient(&mcp.Implementation{Name: "kimi-test", Version: "1"}, nil).Connect(ctx, b, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = cli.Close(); _ = ss.Close(); cancel(); shutdown() }()
	// Ordinary diagnostics must not create a fresh registration.
	out, e := cli.CallTool(ctx, &mcp.CallToolParams{Name: "auth_status", Arguments: map[string]any{}})
	if e != nil || out.IsError {
		t.Fatal(out, e)
	}
	up.mu.Lock()
	count := up.registers
	up.mu.Unlock()
	if count != 0 {
		t.Fatal("registered before explicit attach")
	}
	out, e = cli.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{"name": "kimi-test"}})
	if e != nil || out.IsError {
		t.Fatal(out, e)
	}
	up.mu.Lock()
	s := up.session
	up.mu.Unlock()
	if s.Harness != "kimi" || s.ID != "11111111-1111-4111-8111-111111111111" {
		t.Fatal(s)
	}
	up.messages <- broker.Message{From: "peer", To: s.Name, Body: "waiting-boundary"}
	waitFor(t, "Kimi local queue", func() bool { return k.queue.pendingCount() == 1 })
	out, e = cli.CallTool(ctx, &mcp.CallToolParams{Name: "auth_status", Arguments: map[string]any{}})
	if e != nil || out.IsError {
		t.Fatal(out, e)
	}
	raw, _ := json.Marshal(out.StructuredContent)
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	if fields["delivery"] != "turn-boundary" || fields["pendingMessages"] != float64(1) || fields["registered"] != true || !strings.Contains(fields["message"].(string), kimiDeliveryGuidance) {
		t.Fatal(string(raw))
	}
	// False public readiness must not retire a healthy turn-boundary receiver.
	bind.mu.Lock()
	failed, started := bind.failed, bind.started
	bind.mu.Unlock()
	if failed || !started {
		t.Fatal("false readiness retired Kimi")
	}
	up.mu.Lock()
	count = up.registers
	up.mu.Unlock()
	if count != 1 {
		t.Fatal("Kimi readiness caused re-registration", count)
	}
	_, e = k.accept(kimiHookInput{Event: "Stop", SessionID: kimiTestSession, CWD: cwd})
	if e != nil {
		t.Fatal(e)
	}
	diag, e := state.LoadDiagnostics(bind.durable.key)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, v := range diag.Events {
		found = found || v.Event == "kimi-hook-handoff"
	}
	if !found {
		t.Fatal("missing batch handoff risk diagnostic")
	}
}
func TestKimiMissingCapabilityDoesNotRegister(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cwd := t.TempDir()
	k := newKimiClient(os.Getpid(), cwd)
	k.key, _ = kimiProcessKey(os.Getpid())
	k.checkServer = func(context.Context) error { return errKimiServer }
	_, _ = k.accept(kimiHookInput{Event: "SessionStart", SessionID: kimiTestSession, CWD: cwd})
	state, e := clientstate.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	up := &kimiContractUpstream{}
	server, shutdown := newIngressServerWithOptions(ctx, up, snapshotTools(), k.identify, nil, durableOptions{enabled: true, tokenMode: true, kimi: k, state: state})
	a, b := mcp.NewInMemoryTransports()
	ss, e := server.Connect(ctx, a, nil)
	if e != nil {
		t.Fatal(e)
	}
	cli, e := mcp.NewClient(&mcp.Implementation{Name: "kimi-test", Version: "1"}, nil).Connect(ctx, b, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = cli.Close(); _ = ss.Close(); cancel(); shutdown() }()
	out, e := cli.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{"name": "kimi-test"}})
	if e != nil || !out.IsError {
		t.Fatal("old server accepted", out, e)
	}
	out, e = cli.CallTool(ctx, &mcp.CallToolParams{Name: "auth_status", Arguments: map[string]any{}})
	if e != nil || out.IsError {
		t.Fatal("status unavailable", out, e)
	}
	raw, _ := json.Marshal(out.StructuredContent)
	if !strings.Contains(string(raw), "unsupported-server") {
		t.Fatal(string(raw))
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if up.registers != 0 {
		t.Fatal("registration reached old server")
	}
}

func TestKimiStoredConversationResumesAfterClientRestart(t *testing.T) {
	up := newSwitchableUpstream(t)
	stateDir := t.TempDir()
	configure := func(opts *durableOptions, identify *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) {
		k := newKimiClient(os.Getpid(), stateDir)
		k.key, _ = kimiProcessKey(os.Getpid())
		_, err := k.accept(kimiHookInput{Event: "SessionStart", SessionID: kimiTestSession, CWD: stateDir})
		if err != nil {
			t.Fatal(err)
		}
		opts.kimi, opts.harness = k, "kimi"
		opts.observe = func(ctx context.Context, _ broker.AttachRequest) (broker.AttachRequest, error) {
			return k.identify(ctx, nil)
		}
		*identify = k.identify
	}
	first := newDurableFixtureWithPolicy(t, up, stateDir, true, configure)
	original, result := first.attach(t, "kimi-restart")
	if result.IsError || original.RegistrationID == "" {
		t.Fatal(result)
	}
	first.stop()
	second := newDurableFixtureWithPolicy(t, up, stateDir, true, configure)
	waitFor(t, "stored Kimi resume", func() bool {
		var status authStatus
		if call(t.Context(), second.local, "auth_status", map[string]any{}, &status) != nil {
			return false
		}
		return status.Registered && status.Name == original.Name && status.RegistrationID == original.RegistrationID
	})
	var current broker.Session
	if err := call(t.Context(), up, "get_session", map[string]string{"name": original.Name}, &current); err != nil {
		t.Fatal(err)
	}
	if current.Harness != "kimi" || current.ID != original.ID || current.Generation <= original.Generation {
		t.Fatal("stored conversation did not resume", current)
	}
}
