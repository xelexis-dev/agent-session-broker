package channel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpconfig"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestAuthStatusReportsConnectedVersions(t *testing.T) {
	ctx, cancel := context.WithCancel(mcpconfig.WithVersion(t.Context(), "9.9.9-test"))
	defer cancel()
	clientInfo := make(chan string, 1)
	remote := mcp.NewServer(&mcp.Implementation{Name: "synthetic-broker", Version: "8.8.8-test"}, &mcp.ServerOptions{InitializedHandler: func(_ context.Context, req *mcp.InitializedRequest) {
		clientInfo <- req.Session.InitializeParams().ClientInfo.Version
	}})
	central := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, nil))
	defer central.Close()
	upstream := newBrokerUpstream(ctx, central.URL)
	defer upstream.Close()
	session, err := upstream.session(ctx)
	if err != nil {
		t.Fatal("upstream initialize failed")
	}
	if got := <-clientInfo; got != "9.9.9-test" {
		t.Fatal("upstream clientInfo version wrong")
	}
	if session.InitializeResult().ServerInfo.Version != "8.8.8-test" {
		t.Fatal("server initialize version wrong")
	}
	server, shutdown := newIngressServerWithOptions(ctx, upstream, nil, func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
		return broker.AttachRequest{Harness: "codex", SessionID: "synthetic-thread"}, nil
	}, nil, durableOptions{tokenMode: true})
	defer func() { cancel(); shutdown() }()
	ct, st := mcp.NewInMemoryTransports()
	go server.Run(ctx, st)
	harness, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal("local initialize failed")
	}
	defer harness.Close()
	result, err := harness.CallTool(ctx, &mcp.CallToolParams{Name: "auth_status", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatal("auth status failed")
	}
	raw, _ := json.Marshal(result.StructuredContent)
	var status map[string]any
	if json.Unmarshal(raw, &status) != nil {
		t.Fatal("auth status malformed")
	}
	if status["clientVersion"] != "9.9.9-test" || status["serverVersion"] != "8.8.8-test" || status["serverVersionKnown"] != true {
		t.Fatal("auth_status lacks client/server versions")
	}
	if harness.InitializeResult().ServerInfo.Version != "9.9.9-test" {
		t.Fatal("local initialize version wrong")
	}
}

func TestAuthVersionUnknownLegacyAndReconnect(t *testing.T) {
	ctx := mcpconfig.WithVersion(t.Context(), "")
	var remoteVersion atomic.Value
	remoteVersion.Store("0.1.0")
	central := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return mcp.NewServer(&mcp.Implementation{Name: "test", Version: remoteVersion.Load().(string)}, nil)
	}, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer central.Close()
	u := newBrokerUpstream(ctx, central.URL)
	defer u.Close()
	wrapped := (&durableOptions{}).withDefaults().wrapReceive(u, "")
	out := authVersions(ctx, wrapped, authStatus{})
	if out.ClientVersion != "dev" || out.ServerVersion != "unknown" || out.ServerVersionKnown {
		t.Fatal("unconnected fallback wrong")
	}
	s, err := u.session(ctx)
	if err != nil {
		t.Fatal("connect failed")
	}
	out = authVersions(ctx, wrapped, authStatus{})
	if out.ServerVersion != "0.1.0" || out.ServerVersionKnown {
		t.Fatal("legacy placeholder mistaken for real version")
	}
	// Close the old connection before changing this test server's version.
	s.Close()
	u.mu.Lock()
	u.current = nil
	u.mu.Unlock()
	remoteVersion.Store("8.8.8-test")
	if _, err = u.session(ctx); err != nil {
		t.Fatal("reconnect failed")
	}
	out = authVersions(ctx, wrapped, authStatus{})
	if out.ServerVersion != "8.8.8-test" || !out.ServerVersionKnown {
		t.Fatal("reconnect retained old version")
	}
}

type versionedTestUpstream struct {
	Upstream
	version string
}

func (u versionedTestUpstream) versionInfo() string { return u.version }

func TestAuthVersionsStayWithConversationUpstream(t *testing.T) {
	auth := newDeviceAuthServer(t)
	upstream := newSwitchableUpstream(t)
	const a = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const b = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	identities := map[string]broker.AttachRequest{a: {Harness: "codex", SessionID: a, PID: 7, CWD: `C:\work\a`}, b: {Harness: "codex", SessionID: b, PID: 7, CWD: `C:\work\b`}}
	f := newDeviceModeFixture(t, auth, upstream, t.TempDir(), identities, func(f *deviceModeFixture) {
		f.sharedUpstream = versionedTestUpstream{Upstream: upstream, version: "shared-wrong"}
		f.wrapUpstream = func(credential string, u Upstream) Upstream {
			v := "8.8.8-a"
			if credential == "asb_s_b" {
				v = "8.8.8-b"
			}
			return versionedTestUpstream{Upstream: u, version: v}
		}
	})
	for _, thread := range []string{a, b} {
		s := f.status(thread)
		if s["clientVersion"] != "dev" || s["serverVersion"] != "unknown" || s["serverVersionKnown"] != false {
			t.Fatal("pending conversation inherited shared server version")
		}
	}
	f.grant(a, "asb_s_a", "agent-a")
	f.grant(b, "asb_s_b", "agent-b")
	for thread, version := range map[string]string{a: "8.8.8-a", b: "8.8.8-b"} {
		waitFor(t, "own registered version", func() bool { return f.status(thread)["registered"] == true })
		s := f.status(thread)
		if s["serverVersion"] != version || s["serverVersionKnown"] != true {
			t.Fatal("conversation reported another upstream version")
		}
	}
}

func TestMissingUpstreamServerInfoIsUnknown(t *testing.T) {
	central := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var request struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(400)
			return
		}
		if request.Method == "server/discover" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32601, "message": "unsupported"}})
			return
		}
		if request.Method != "initialize" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}}})
	}))
	defer central.Close()
	upstream := newBrokerUpstream(t.Context(), central.URL)
	defer upstream.Close()
	defer func() {
		if recover() != nil {
			t.Fatal("missing serverInfo panicked the client")
		}
	}()
	if _, err := upstream.session(t.Context()); err != nil {
		t.Fatal("synthetic initialize failed")
	}
	status := authVersions(t.Context(), upstream, authStatus{})
	if status.ServerVersion != "unknown" || status.ServerVersionKnown {
		t.Fatal("missing serverInfo was not unknown")
	}
}
