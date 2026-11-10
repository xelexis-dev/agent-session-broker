package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

func TestBrokerUpstreamReconnectsWithoutReplayingMutation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	central := recoveryBroker(t)
	var sends atomic.Int32
	var loseReply atomic.Bool
	loseReply.Store(true)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var msg struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &msg)
		if msg.Params.Name == "send_message" {
			sends.Add(1)
			if loseReply.CompareAndSwap(true, false) {
				// Execute the mutation, then lose its response. A transparent
				// retry would enqueue the same message twice.
				central.Config.Handler.ServeHTTP(httptest.NewRecorder(), r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":`))
				w.(http.Flusher).Flush()
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				conn.Close()
				return
			}
		}
		central.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	up := newTestBrokerUpstream(ctx, proxy.URL+"/mcp?project=brk")
	t.Cleanup(func() { up.Close() })
	id := broker.AttachRequest{Harness: "codex", Name: "owner", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: `C:\work`, ChannelID: "original-lease"}
	if err := call(ctx, up, "attach_session", id, nil); err != nil {
		t.Fatal(err)
	}
	if err := call(ctx, up, "send_message", broker.Message{From: "owner", To: "owner", Body: "exactly once"}, nil); err == nil {
		t.Fatal("lost response reported as success")
	}
	var got broker.Session
	if err := call(ctx, up, "get_session", map[string]string{"name": "owner"}, &got); err != nil {
		t.Fatalf("subsequent call did not recover: %v", err)
	}
	if sends.Load() != 1 || got.PendingMessages != 1 {
		t.Fatalf("mutation replayed: calls=%d pending=%d", sends.Load(), got.PendingMessages)
	}
	up.Close()
	if err := call(ctx, up, "get_session", map[string]string{"name": "owner"}, nil); err == nil {
		t.Fatal("closed client reconnected")
	}
}

func TestIngressRecoversAfterHTTPDisconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	central := recoveryBroker(t)
	var offline atomic.Bool
	waitStarted := make(chan struct{}, 8)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte(`"wait_for_message"`)) {
			select {
			case waitStarted <- struct{}{}:
			default:
			}
		}
		central.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	up := newTestBrokerUpstream(ctx, proxy.URL+"/mcp?project=brk")
	t.Cleanup(func() { up.Close() })
	delivered := make(chan broker.Message, 2)
	local := recoveryLocal(t, ctx, up, func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
		return broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: `C:\work`}, nil
	}, func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
		return func(_ context.Context, m broker.Message) error { delivered <- m; return nil }, nil, nil
	})
	var first broker.Session
	if err := call(ctx, local, "attach_session", map[string]string{"name": "owner"}, &first); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waitStarted:
	case <-ctx.Done():
		t.Fatal("wait did not start")
	}
	offline.Store(true)
	proxy.CloseClientConnections()
	if err := call(ctx, up, "get_session", map[string]string{"name": "owner"}, nil); err == nil {
		t.Fatal("offline request succeeded")
	}
	offline.Store(false)
	// Retire the unreachable lease, as the server does on heartbeat expiry.
	// Separate admin connection avoids simulating expiry by sleeping 30 seconds.
	admin, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.17.3"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: central.URL + "/mcp?project=brk"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	if err := call(ctx, admin, "stop_session", map[string]string{"name": "owner"}, nil); err != nil {
		t.Fatal(err)
	}
	var next broker.Session
	for {
		err = call(ctx, local, "attach_session", map[string]string{"name": "owner"}, &next)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("same local MCP did not recover: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if next.Generation <= first.Generation || !next.AutomaticDeliveryReady {
		t.Fatalf("lease not replaced: %+v", next)
	}
	if err := call(ctx, admin, "send_message", broker.Message{From: "owner", To: "owner", Body: "network restored"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-delivered:
		if m.Body != "network restored" {
			t.Fatal(m)
		}
	case <-ctx.Done():
		t.Fatal("recovered receiver did not deliver")
	}
}
