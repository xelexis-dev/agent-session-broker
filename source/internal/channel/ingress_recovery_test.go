package channel

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
)

func recoveryBroker(t *testing.T) *httptest.Server {
	t.Helper()
	r, err := groups.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	f := mcpserver.NewRemoteProjectFactory(r)
	t.Cleanup(func() { f.Close() })
	h := httptest.NewServer(mcpserver.ProjectHandler(f))
	t.Cleanup(h.Close)
	return h
}

func recoveryLocal(t *testing.T, ctx context.Context, up Upstream, identify func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error), factory ingressSinkFactory) *mcp.ClientSession {
	t.Helper()
	s := newIngressServer(ctx, up, nil, identify, factory)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	c, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.17.3"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// A failed receiver must not permanently poison name-only registration on the
// same stdio MCP connection. The remote service and both MCP layers are real;
// the native agent process is replaced by a sink to avoid messaging user sessions.
func TestIngressReregistersRetiredReceiver(t *testing.T) {
	for _, scenario := range []struct{ harness, name string }{
		{"claude", "owner"}, {"claude", "renamed-owner"},
		{"codex", "owner"}, {"codex", "renamed-owner"},
	} {
		t.Run(scenario.harness+"/"+scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			h := recoveryBroker(t)
			up, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.17.3"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: h.URL + "/mcp?project=brk"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { up.Close() })
			t.Cleanup(cancel)
			var pid atomic.Int64
			pid.Store(123)
			var created atomic.Int32
			delivered := make(chan broker.Message, 2)
			local := recoveryLocal(t, ctx, up, func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
				return broker.AttachRequest{Harness: scenario.harness, SessionID: "11111111-1111-4111-8111-111111111111", PID: int(pid.Load()), CWD: `C:\work`}, nil
			}, func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
				created.Add(1)
				return func(_ context.Context, m broker.Message) error { delivered <- m; return nil }, nil, nil
			})
			defer cancel()
			var first broker.Session
			if err := call(ctx, local, "attach_session", map[string]string{"name": "owner"}, &first); err != nil {
				t.Fatal(err)
			}
			if err := call(ctx, up, "stop_session", map[string]string{"name": "owner"}, nil); err != nil {
				t.Fatal(err)
			}
			pid.Store(456)
			if err := call(ctx, local, "attach_session", map[string]string{"name": "owner"}, nil); err == nil {
				t.Fatal("changed parent was accepted")
			}
			pid.Store(123)
			var next broker.Session
			deadline := time.Now().Add(time.Second)
			for {
				err = call(ctx, local, "attach_session", map[string]string{"name": scenario.name}, &next)
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("same local MCP could not re-register after receiver retirement: %v", err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !next.AutomaticDeliveryReady || next.Generation <= first.Generation || created.Load() != 2 {
				t.Fatalf("fresh receiver missing: %+v, factories=%d", next, created.Load())
			}
			if err := call(ctx, up, "send_message", broker.Message{From: scenario.name, To: scenario.name, Body: "after recovery"}, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case m := <-delivered:
				if m.Body != "after recovery" {
					t.Fatal(m)
				}
			case <-time.After(time.Second):
				t.Fatal("re-registered receiver did not deliver")
			}
		})
	}
}

func TestIngressShutdownWaitsForSink(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := &ingressUpstream{input: make(chan broker.Message, 1)}
	up.input <- broker.Message{To: "owner", Body: "in flight"}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	go func() {
		receiveIngress(ctx, up, broker.AttachRequest{Name: "owner"}, func(context.Context, broker.Message) error {
			close(entered)
			<-release
			return fmt.Errorf("sink ended")
		})
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("sink did not start")
	}
	cancel()
	select {
	case <-done:
		t.Fatal("receiver reported completion while its old sink was still running")
	case <-time.After(50 * time.Millisecond):
	}
}
