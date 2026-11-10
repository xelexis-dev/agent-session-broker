package channel

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpconfig"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientCompatibilityDegraded(t *testing.T) {
	var calls atomic.Int32
	remote := mcp.NewServer(&mcp.Implementation{Name: "old", Version: "0.11.9"}, nil)
	for _, name := range []string{"list_sessions", "get_session", "send_message", "register_session"} {
		remote.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			calls.Add(1)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "[]"}}}, nil
		})
	}
	central := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, nil))
	defer central.Close()
	ctx := mcpconfig.WithVersion(t.Context(), "1.0.0")
	u := newBrokerUpstream(ctx, central.URL)
	defer u.Close()
	if _, e := u.session(ctx); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		r, e := u.CallTool(ctx, &mcp.CallToolParams{Name: "register_session", Arguments: map[string]any{}})
		if e != nil || !r.IsError || !strings.Contains(r.Content[0].(*mcp.TextContent).Text, "server-incompatible") {
			t.Fatal("missing compatibility rejection")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("incompatible registration reached server")
	}
	for _, name := range []string{"list_sessions", "get_session"} {
		r, e := u.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		if e != nil || r.IsError {
			t.Fatal("degraded read failed")
		}
	}
	out := authVersions(ctx, u, authStatus{})
	if out.Compatible || out.RequiredServer != "0.17.3" || !strings.Contains(out.Message, "server-incompatible") {
		t.Fatal("auth compatibility missing")
	}
	if e := call(ctx, u, "register_session", nil, nil); !isCompatibilityError(e) {
		t.Fatal("compatibility error lost")
	}
}
func TestClientCompatibilityServerVersions(t *testing.T) {
	for _, tc := range []struct {
		version string
		ok      bool
	}{{"0.17.3", true}, {"0.12.0", false}, {"0.1.0", false}, {"dev", true}, {"test", true}, {"0.11.9", false}} {
		if state := assessCompatibility("1.0.0", tc.version); state.Compatible != tc.ok {
			t.Fatal("server version assessment", tc.version)
		}
	}
}

func TestClientLearnsGateAndStopsRegistration(t *testing.T) {
	var registrations atomic.Int32
	guidance := protocol.CompatibilityGuidance("client-incompatible", "1.0.0", "0.18.0", "0.18.0")
	remote := mcp.NewServer(&mcp.Implementation{Name: "future", Version: "0.18.0"}, nil)
	remote.AddTool(&mcp.Tool{Name: "register_session", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		registrations.Add(1)
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: guidance}}}, nil
	})
	central := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, nil))
	defer central.Close()
	ctx := mcpconfig.WithVersion(t.Context(), "1.0.0")
	u := newBrokerUpstream(ctx, central.URL)
	defer u.Close()
	for i := 0; i < 3; i++ {
		if e := call(ctx, u, "register_session", map[string]any{}, nil); !isCompatibilityError(e) {
			t.Errorf("gate error did not become a compatibility error")
		}
	}
	state := u.compatibilityInfo()
	if state.Compatible || state.RequiredClient != "0.18.0" || state.Message != guidance {
		t.Error("gate response did not enter degraded mode")
	}
	if registrations.Load() != 1 {
		t.Errorf("registration attempts = %d, want 1", registrations.Load())
	}
	out := authVersions(ctx, u, authStatus{})
	if out.Compatible || out.RequiredClient != "0.18.0" {
		t.Error("auth_status missing gate requirement")
	}
}

func TestGateGuidanceSanitized(t *testing.T) {
	valid := protocol.CompatibilityGuidance("client-incompatible", "1.0.0", "0.18.0", "0.18.0")
	for _, text := range []string{valid, "client-incompatible: synthetic-secret", strings.Replace(valid, "0.18.0", "http://synthetic-secret", 1), valid + " synthetic-secret"} {
		r := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}
		state, blocked := gateCompatibility(r, "1.0.0", "0.18.0", compatibilityState{Compatible: true, RequiredServer: "0.12.0"})
		if !blocked || state.Compatible || strings.Contains(state.Message, "synthetic-secret") {
			t.Fatal("unsafe gate response handling")
		}
		if text == valid && state.RequiredClient != "0.18.0" {
			t.Fatal("lost parsed minimum")
		}
		if text != valid && state.RequiredClient != "" {
			t.Fatal("trusted malformed minimum")
		}
	}
	for _, r := range []*mcp.CallToolResult{nil, {IsError: false, Content: []mcp.Content{&mcp.TextContent{Text: valid}}}, {IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "other-error"}}}} {
		if _, blocked := gateCompatibility(r, "1.0.0", "0.18.0", compatibilityState{Compatible: true}); blocked {
			t.Fatal("non-gate result degraded client")
		}
	}
}

func TestAutomaticGateFailureKeepsDiagnosticsAndStopsRetry(t *testing.T) {
	var registrations atomic.Int32
	guidance := protocol.CompatibilityGuidance("client-incompatible", "1.0.0", "0.18.0", "0.18.0")
	remote := mcp.NewServer(&mcp.Implementation{Name: "future", Version: "0.18.0"}, nil)
	remote.AddTool(&mcp.Tool{Name: "register_session", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		registrations.Add(1)
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: guidance}}}, nil
	})
	remote.AddTool(&mcp.Tool{Name: "list_sessions", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "[]"}}}, nil
	})
	central := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, nil))
	defer central.Close()
	ctx, cancel := context.WithCancel(mcpconfig.WithVersion(t.Context(), "1.0.0"))
	defer cancel()
	u := newBrokerUpstream(ctx, central.URL)
	defer u.Close()
	state, e := clientstate.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	identity := protocol.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 4242, CWD: t.TempDir()}
	var retries atomic.Int32
	server, shutdown := newIngressServerWithOptions(ctx, u, snapshotTools(), func(context.Context, *mcp.CallToolRequest) (protocol.AttachRequest, error) { return identity, nil }, func(protocol.AttachRequest) (func(context.Context, protocol.Message) error, *codexIngress, error) {
		return func(context.Context, protocol.Message) error { return nil }, nil, nil
	}, durableOptions{enabled: true, tokenMode: true, state: state, scope: central.URL, project: "test", observe: func(context.Context, protocol.AttachRequest) (protocol.AttachRequest, error) { return identity, nil }, backoff: func(int) time.Duration { retries.Add(1); return time.Millisecond }})
	defer func() { cancel(); shutdown() }()
	st, ct := mcp.NewInMemoryTransports()
	ss, e := server.Connect(ctx, st, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ss.Close()
	cs, e := mcp.NewClient(&mcp.Implementation{Name: "host", Version: "1"}, nil).Connect(ctx, ct, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer cs.Close()
	// A fresh conversation reaches registration only after explicit attach.
	_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
	waitFor(t, "automatic compatibility gate", func() bool { return !u.compatibilityInfo().Compatible })
	for _, name := range []string{"send_message", "attach_session", "rename_session", "memory_put"} {
		r, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		if e != nil || !r.IsError || r.Content[0].(*mcp.TextContent).Text != guidance {
			t.Fatalf("%s did not show compatibility guidance", name)
		}
	}
	r, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "auth_status", Arguments: map[string]any{}})
	if e != nil || r.IsError {
		t.Fatal("auth_status unavailable")
	}
	raw, _ := json.Marshal(r.StructuredContent)
	var auth authStatus
	if json.Unmarshal(raw, &auth) != nil || auth.Compatible || auth.RequiredClient != "0.18.0" {
		t.Fatal("auth_status compatibility fields")
	}
	r, e = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
	if e != nil || r.IsError {
		t.Fatal("degraded list unavailable")
	}
	cancel()
	shutdown()
	if registrations.Load() != 1 || retries.Load() != 0 {
		t.Fatalf("register calls=%d backoff=%d", registrations.Load(), retries.Load())
	}
}

func TestOneClientMinimumServer(t *testing.T) {
	for _, v := range []string{"0.17.2", "0.1.0"} {
		s := assessCompatibility("1.0.0", v)
		if s.Compatible || s.Code != "server-incompatible" || s.RequiredServer != "0.17.3" {
			t.Error("wrong minimum-server rejection", v)
		}
	}
	for _, v := range []string{"0.17.3", "0.19.2", "0.20.0", "1.0.0"} {
		if !assessCompatibility("1.0.0", v).Compatible {
			t.Error("supported server rejected", v)
		}
	}
}
