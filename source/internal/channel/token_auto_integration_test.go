package channel

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverruntime"
)

// This fixture uses the production HTTP runtime and SQLite policy/registration
// stores. Only the local harness observation and delivery sink are substituted;
// no operational token, process, or broker participates in the test.
func TestTokenExplicitAttachConfirmsRegistrationAndReception(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root := t.TempDir()
	store, err := controlpanel.OpenSQLite(filepath.Join(root, "control"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Setup("integration-fixture-password"); err != nil {
		t.Fatal(err)
	}
	if err = store.SaveProject("automatic", "token", true, ""); err != nil {
		t.Fatal(err)
	}
	if err = store.SetInstanceHTTPPolicy("admin", true); err != nil {
		t.Fatal(err)
	}
	token, err := store.IssueToken("automatic", "connected-session")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	// Reserve an ephemeral loopback port; Run owns its listener and closes it.
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	reservation.Close()
	serverCtx, stopServer := context.WithCancel(ctx)
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serverruntime.Run(serverCtx, []string{"--listen", address, "--memory-dir", root}, "test", io.Discard)
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			stopServer()
			select {
			case err := <-serverDone:
				if err != nil {
					t.Errorf("server shutdown: %v", err)
				}
			case <-time.After(7 * time.Second):
				t.Error("server did not stop")
			}
		})
	}
	t.Cleanup(stop)
	base := "http://" + address
	httpClient := &http.Client{Timeout: time.Second}
	waitFor(t, "runtime health", func() bool {
		res, err := httpClient.Get(base + "/healthz")
		if err != nil {
			return false
		}
		defer res.Body.Close()
		return res.StatusCode == http.StatusOK
	})
	endpoint := base + "/mcp?project=automatic"
	unauthorized, err := httpClient.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous MCP returned %d", unauthorized.StatusCode)
	}
	upstream := newTestBrokerUpstream(ctx, endpoint)
	upstream.setBearerToken(token)
	t.Cleanup(func() { upstream.Close() })
	remote, err := upstream.session(ctx)
	if err != nil {
		t.Fatal("authenticated handshake failed")
	}
	listed, err := remote.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	identity := broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111112237", PID: 2237, CWD: t.TempDir()}
	sink := make(chan broker.Message, 1)
	localCtx, stopLocal := context.WithCancel(ctx)
	options := durableOptions{enabled: true, tokenMode: true, state: state, scope: brokerScope(endpoint, "automatic"), project: "automatic", heartbeat: 50 * time.Millisecond, timeout: time.Second, backoff: func(int) time.Duration { return 10 * time.Millisecond }}
	options.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return identity, nil }
	local, shutdown := newIngressServerWithOptions(localCtx, upstream, listed.Tools,
		func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) { return identity, nil },
		func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
			return func(ctx context.Context, message broker.Message) error {
				select {
				case sink <- message:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}, nil, nil
		}, options)
	t.Cleanup(shutdown)
	t.Cleanup(stopLocal)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := local.Connect(localCtx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	harness, err := mcp.NewClient(&mcp.Implementation{Name: "harness", Version: "test"}, nil).Connect(localCtx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { harness.Close() })
	// Explicit attach starts the first registration; the handshake alone does not.
	_, _ = harness.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
	var registered broker.Session
	waitFor(t, "automatic token registration", func() bool {
		return call(ctx, upstream, "get_session", map[string]string{"name": "connected-session"}, &registered) == nil && registered.AutomaticDeliveryReady
	})
	if registered.ID != identity.SessionID || registered.Name != "connected-session" || registered.RegistrationID == "" {
		t.Fatalf("wrong conversation bound: %+v", registered)
	}
	if err = call(ctx, upstream, "send_message", broker.Message{From: registered.Name, To: registered.Name, Body: "connection-only nonce"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case delivered := <-sink:
		if delivered.Body != "connection-only nonce" {
			t.Fatal("wrong payload delivered")
		}
	case <-ctx.Done():
		t.Fatal("automatically registered receiver did not deliver")
	}
	stopLocal()
	shutdown()
	harness.Close()
	ss.Close()
	upstream.Close()
	stop()
	reopened, err := controlpanel.OpenSQLite(filepath.Join(root, "control"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	projects := reopened.Projects()
	if len(projects) != 1 || len(projects[0].Tokens) != 1 || projects[0].Tokens[0].FirstConnectedAt.IsZero() || projects[0].Tokens[0].BoundIdentity == "" {
		t.Fatal("successful automatic registration did not persist token connection status")
	}
}
