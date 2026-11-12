package channel

import (
	"context"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	rb "github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpconfig"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestServerVersionRefreshAfterStatelessSwap(t *testing.T) {
	ctx := mcpconfig.WithVersion(t.Context(), "1.0.4")
	backend := rb.NewRemoteService()
	defer backend.Close()
	handlers := []http.Handler{mcpserver.Handler(mcpserver.New(backend, noMemory{}, "1.0.3")), mcpserver.Handler(mcpserver.New(backend, noMemory{}, "1.0.4"))}
	var current atomic.Int32
	var fail atomic.Bool
	var requests [2]atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		index := current.Load()
		requests[index].Add(1)
		handlers[index].ServeHTTP(w, r)
	}))
	defer server.Close()
	u := newBrokerUpstream(ctx, server.URL+"/mcp")
	defer u.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	var connections atomic.Int32
	dialer := &net.Dialer{}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		connections.Add(1)
		return dialer.DialContext(ctx, network, address)
	}
	u.transport = transport
	req := broker.RegisterRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 77, CWD: "/fixture", ClientID: "fixture-client-1", Name: "fixture", ChannelID: "fixture-lease", Secret: strings.Repeat("f", 40), RequestID: "fixture-register"}
	var registered broker.RegisterResult
	if err := call(ctx, u, "register_session", req, &registered); err != nil || registered.Code != "" {
		t.Fatalf("register failed: %v %s", err, registered.Code)
	}
	session, _ := u.session(ctx)
	before := authVersions(ctx, u, authStatus{})
	if before.ServerVersion != "1.0.3" {
		t.Fatal("baseline version")
	}
	oldConnections := connections.Load()
	stopReads := make(chan struct{})
	readsDone := make(chan struct{})
	go func() {
		defer close(readsDone)
		for {
			select {
			case <-stopReads:
				return
			default:
				_ = authVersions(ctx, u, authStatus{})
			}
		}
	}()
	defer func() { close(stopReads); <-readsDone }()
	current.Store(1)
	transport.CloseIdleConnections()
	var renewed broker.RegisterResult
	if err := call(ctx, u, "renew_lease", broker.RenewRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: req.ChannelID}, &renewed); err != nil || renewed.Code != "" {
		t.Fatalf("renew after swap: %v %s", err, renewed.Code)
	}
	after := authVersions(ctx, u, authStatus{})
	same, _ := u.session(ctx)
	if connections.Load() <= oldConnections || requests[1].Load() == 0 || after.ServerVersion != "1.0.4" || same != session {
		t.Fatal("successful new-server renewal retained stale version")
	}
	t.Logf("successful renewal on replaced server updates client %s to %s", after.ClientVersion, after.ServerVersion)
}

func TestServerVersionReplyCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{"old-server", map[string]any{"session": map[string]any{}}, "1.0.3"},
		{"new-server", map[string]any{"session": map[string]any{}, "serverVersion": "1.0.4"}, "1.0.4"},
		{"refusal", map[string]any{"code": "owned", "serverVersion": "1.0.4"}, "1.0.3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0.3"}, nil)
			for _, name := range []string{"register_session", "resume_session", "renew_lease"} {
				mcp.AddTool(remote, &mcp.Tool{Name: name}, func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, any, error) {
					return nil, tc.payload, nil
				})
			}
			server := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, &mcp.StreamableHTTPOptions{Stateless: true}))
			defer server.Close()
			u := newBrokerUpstream(mcpconfig.WithVersion(t.Context(), "1.0.4"), server.URL)
			defer u.Close()
			if _, err := u.session(t.Context()); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"register_session", "resume_session", "renew_lease"} {
				u.mu.Lock()
				u.serverVersion = "1.0.3"
				u.mu.Unlock()
				if _, err := u.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{}}); err != nil {
					t.Fatal(err)
				}
				if got := authVersions(t.Context(), u, authStatus{}).ServerVersion; got != tc.want {
					t.Errorf("%s got %s want %s", name, got, tc.want)
				}
			}
		})
	}
}

func TestServerVersionReplyRejectsMalformedSignals(t *testing.T) {
	for _, result := range []*mcp.CallToolResult{
		nil, {IsError: true, StructuredContent: map[string]any{"serverVersion": "1.0.4"}},
		{StructuredContent: map[string]any{"code": "owned", "serverVersion": "1.0.4"}},
		{StructuredContent: map[string]any{"serverVersion": 123}},
		{StructuredContent: map[string]any{"serverVersion": " \t"}},
		{StructuredContent: make(chan int)}, {},
	} {
		if version, successful := replyServerVersion(result); version != "" || successful {
			t.Error("malformed/error/absent version accepted")
		}
	}
}

type heldVersionRenewBackend struct {
	*rb.RemoteService
	entered, release chan struct{}
}

func (b *heldVersionRenewBackend) RenewLease(ctx context.Context, in broker.RenewRequest) (broker.RegisterResult, error) {
	out, err := b.RemoteService.RenewLease(ctx, in)
	close(b.entered)
	select {
	case <-b.release:
		return out, err
	case <-ctx.Done():
		return broker.RegisterResult{}, ctx.Err()
	}
}

func TestServerVersionOldReplyAfterReconnectIsFenced(t *testing.T) {
	ctx := mcpconfig.WithVersion(t.Context(), "1.0.4")
	backend := rb.NewRemoteService()
	defer backend.Close()
	held := &heldVersionRenewBackend{RemoteService: backend, entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(held.release) }) }
	defer release()
	handlers := []http.Handler{mcpserver.Handler(mcpserver.New(held, noMemory{}, "1.0.4")), mcpserver.Handler(mcpserver.New(backend, noMemory{}, "1.0.5"))}
	var current atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handlers[current.Load()].ServeHTTP(w, r) }))
	defer server.Close()
	u := newBrokerUpstream(ctx, server.URL+"/mcp")
	defer u.Close()
	// Release a held response before httptest's shutdown even if an assertion fails.
	defer release()
	var registered broker.RegisterResult
	req := broker.RegisterRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 77, CWD: "/fixture", ClientID: "fixture-client-1", Name: "fixture", ChannelID: "fixture-lease", Secret: strings.Repeat("f", 40), RequestID: "fixture-register"}
	if err := call(ctx, u, "register_session", req, &registered); err != nil || registered.Code != "" {
		t.Fatal("register fixture")
	}
	old, err := u.session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	done := make(chan error, 1)
	go func() {
		var out broker.RegisterResult
		done <- call(ctx, u, "renew_lease", broker.RenewRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: req.ChannelID}, &out)
	}()
	select {
	case <-held.entered:
	case err := <-done:
		t.Fatalf("renew returned before hold: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("renew was not held")
	}
	current.Store(1)
	u.mu.Lock()
	u.current = nil
	u.mu.Unlock()
	if _, err := u.session(ctx); err != nil {
		t.Fatal(err)
	}
	if got := authVersions(ctx, u, authStatus{}).ServerVersion; got != "1.0.5" {
		t.Fatalf("reconnect version %s", got)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := authVersions(ctx, u, authStatus{}).ServerVersion; got != "1.0.5" {
		t.Fatalf("old reply rewound new connection: %s", got)
	}
}

func TestServerVersionOlderSameSessionReplyIsFenced(t *testing.T) {
	ctx := mcpconfig.WithVersion(t.Context(), "1.0.4")
	backend := rb.NewRemoteService()
	defer backend.Close()
	held := &heldVersionRenewBackend{RemoteService: backend, entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(held.release) }) }
	defer release()
	handlers := []http.Handler{mcpserver.Handler(mcpserver.New(held, noMemory{}, "1.0.4")), mcpserver.Handler(mcpserver.New(backend, noMemory{}, "1.0.5"))}
	var current atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handlers[current.Load()].ServeHTTP(w, r) }))
	defer server.Close()
	u := newBrokerUpstream(ctx, server.URL+"/mcp")
	defer u.Close()
	// Release a held response before httptest's shutdown even if an assertion fails.
	defer release()
	var registered broker.RegisterResult
	req := broker.RegisterRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 77, CWD: "/fixture", ClientID: "fixture-client-1", Name: "fixture", ChannelID: "fixture-lease", Secret: strings.Repeat("f", 40), RequestID: "fixture-register"}
	if err := call(ctx, u, "register_session", req, &registered); err != nil || registered.Code != "" {
		t.Fatal("register fixture")
	}
	old, err := u.session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	done := make(chan error, 1)
	go func() {
		var out broker.RegisterResult
		done <- call(ctx, u, "renew_lease", broker.RenewRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: req.ChannelID}, &out)
	}()
	select {
	case <-held.entered:
	case err := <-done:
		t.Fatalf("renew returned before hold: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("renew was not held")
	}
	current.Store(1)
	var newer broker.RegisterResult
	if err := call(ctx, u, "renew_lease", broker.RenewRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: req.ChannelID}, &newer); err != nil || newer.Code != "" {
		t.Fatalf("new renew: %v %s", err, newer.Code)
	}
	if same, _ := u.session(ctx); same != old {
		t.Fatal("unexpected reconnect")
	}
	if got := authVersions(ctx, u, authStatus{}).ServerVersion; got != "1.0.5" {
		t.Fatalf("reconnect version %s", got)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := authVersions(ctx, u, authStatus{}).ServerVersion; got != "1.0.5" {
		t.Fatalf("old reply rewound new connection: %s", got)
	}
}

func TestServerVersionOmissionAfterRefreshIsUnknown(t *testing.T) {
	ctx := mcpconfig.WithVersion(t.Context(), "1.0.4")
	var current atomic.Int32
	handlers := make([]http.Handler, 4)
	for i, version := range []string{"1.0.3", "1.0.5", "1.0.4", "1.0.4"} {
		remote := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: version}, nil)
		payload := map[string]any{"session": map[string]any{}}
		if i == 1 {
			payload["serverVersion"] = version
		}
		mcp.AddTool(remote, &mcp.Tool{Name: "renew_lease"}, func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, any, error) {
			if i == 2 {
				return nil, map[string]any{"code": "owned"}, nil
			}
			if i == 3 {
				return &mcp.CallToolResult{IsError: true}, map[string]any{"serverVersion": "1.0.4"}, nil
			}
			return nil, payload, nil
		})
		handlers[i] = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, &mcp.StreamableHTTPOptions{Stateless: true})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handlers[current.Load()].ServeHTTP(w, r) }))
	defer server.Close()
	u := newBrokerUpstream(ctx, server.URL)
	defer u.Close()
	session, err := u.session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int32{1, 2, 3, 0} {
		current.Store(index)
		if _, err := u.CallTool(ctx, &mcp.CallToolParams{Name: "renew_lease", Arguments: map[string]any{}}); err != nil {
			t.Fatal(err)
		}
		if same, _ := u.session(ctx); same != session {
			t.Fatal("unexpected reconnect")
		}
		if index != 0 {
			if got := authVersions(ctx, u, authStatus{}); got.ServerVersion != "1.0.5" || !got.ServerVersionKnown {
				t.Fatalf("refusal/error changed version: %s known=%v", got.ServerVersion, got.ServerVersionKnown)
			}
		}
	}
	if got := authVersions(ctx, u, authStatus{}); got.ServerVersion != "unknown" || got.ServerVersionKnown {
		t.Fatalf("omitted metadata must become unknown, got version=%s known=%v", got.ServerVersion, got.ServerVersionKnown)
	}
	u.mu.Lock()
	u.current = nil
	u.mu.Unlock()
	defer session.Close()
	if _, err := u.session(ctx); err != nil {
		t.Fatal(err)
	}
	if got := authVersions(ctx, u, authStatus{}); got.ServerVersion != "1.0.3" || !got.ServerVersionKnown {
		t.Fatalf("reconnect must refresh unknown: %s known=%v", got.ServerVersion, got.ServerVersionKnown)
	}

}

func TestServerVersionOmissionOrdering(t *testing.T) {
	for _, startWithMetadata := range []bool{false, true} {
		t.Run(fmt.Sprint(startWithMetadata), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			var requests atomic.Int32
			remote := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0.3"}, nil)
			holdIndex := int32(0)
			if startWithMetadata {
				holdIndex = 1
			}
			mcp.AddTool(remote, &mcp.Tool{Name: "renew_lease"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
				index := requests.Add(1) - 1
				payload := map[string]any{"session": map[string]any{}}
				if startWithMetadata && index == 0 || !startWithMetadata && index > 0 {
					payload["serverVersion"] = "1.0.5"
				}
				if index == holdIndex {
					if startWithMetadata {
						payload["serverVersion"] = "1.0.4"
					}
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return nil, nil, ctx.Err()
					}
				}
				return nil, payload, nil
			})
			server := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, &mcp.StreamableHTTPOptions{Stateless: true}))
			defer server.Close()
			u := newBrokerUpstream(t.Context(), server.URL)
			defer u.Close()
			defer unblock()
			renew := func() error {
				_, err := u.CallTool(t.Context(), &mcp.CallToolParams{Name: "renew_lease", Arguments: map[string]any{}})
				return err
			}
			if startWithMetadata {
				if err := renew(); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() { done <- renew() }()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("old request not held: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("old request not held")
			}
			if err := renew(); err != nil {
				t.Fatal(err)
			}
			unblock()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			want := "1.0.5"
			known := true
			if startWithMetadata {
				want = "unknown"
				known = false
			}
			if got := authVersions(t.Context(), u, authStatus{}); got.ServerVersion != want || got.ServerVersionKnown != known {
				t.Fatalf("older observation changed latest state: %s known=%v", got.ServerVersion, got.ServerVersionKnown)
			}
		})
	}
}

func TestVersionUnknownGuidance(t *testing.T) {
	if !strings.Contains(versionInstructions, "missing version metadata") || !strings.Contains(versionInstructions, "reconnect MCP") || strings.Contains(versionInstructions, "unknown means no upstream connection") {
		t.Fatal("auth_status guidance excludes metadata loss")
	}
	raw, err := clientDocs.ReadFile("docs/upgrade.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"serverVersionKnown: false", "even while connected", "Reconnect MCP"} {
		if !strings.Contains(string(raw), phrase) {
			t.Errorf("upgrade docs omit %s", phrase)
		}
	}
}
