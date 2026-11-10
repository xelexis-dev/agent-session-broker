package channel

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
)

func TestRemoteEndpointValidation(t *testing.T) {
	for _, url := range []string{"http://192.0.2.20:8787/mcp?project=brk", "http://broker:8787/mcp?project=brk", "https://broker.example/mcp?project=brk"} {
		if err := validateBrokerURL(url); err != nil {
			t.Fatal(url, err)
		}
	}
	for _, url := range []string{"http:///mcp?project=brk", "ftp://broker/mcp?project=brk", "http://user:pass@broker/mcp?project=brk", "http://broker/mcp?project=brk&project=other", "http://broker/mcp?project=../escape", "http://broker/mcp?project=brk#fragment"} {
		if err := validateBrokerURL(url); err == nil {
			t.Fatal("invalid endpoint accepted", url)
		}
	}
}

// A has no CLI adapters. B and C each use the real local ingress MCP server;
// only native harness delivery is replaced with an observable sink.
func TestRemoteMCPRelaysDeliverBothDirections(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("TLS=%v", secure), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			registry, err := groups.NewRegistry(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer registry.Close()
			factory := mcpserver.NewRemoteProjectFactory(registry)
			defer factory.Close()
			var central *httptest.Server
			if secure {
				central = httptest.NewTLSServer(mcpserver.ProjectHandler(factory))
			} else {
				central = httptest.NewServer(mcpserver.ProjectHandler(factory))
			}
			defer central.Close()
			connect := func(project string) *mcp.ClientSession {
				c, e := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.17.3"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: central.URL + "/mcp?project=" + project, HTTPClient: central.Client()}, nil)
				if e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() { c.Close() })
				return c
			}
			messages := []chan broker.Message{make(chan broker.Message, 1), make(chan broker.Message, 1)}
			locals := make([]*mcp.ClientSession, 2)
			for i, harness := range []string{"codex", "claude"} {
				upstream := connect("brk")
				listed, e := upstream.ListTools(ctx, nil)
				if e != nil {
					t.Fatal(e)
				}
				identity := broker.AttachRequest{Harness: harness, SessionID: fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1), PID: 1234, CWD: `C:\remote\brk`}
				sink := messages[i]
				server := newIngressServer(ctx, upstream, listed.Tools, func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) { return identity, nil }, func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
					return func(_ context.Context, m broker.Message) error { sink <- m; return nil }, nil, nil
				})
				st, ct := mcp.NewInMemoryTransports()
				ss, e := server.Connect(ctx, st, nil)
				if e != nil {
					t.Fatal(e)
				}
				defer ss.Close()
				local, e := mcp.NewClient(&mcp.Implementation{Name: harness, Version: "1"}, nil).Connect(ctx, ct, nil)
				if e != nil {
					t.Fatal(e)
				}
				defer local.Close()
				locals[i] = local
				var got broker.Session
				if e = call(ctx, local, "attach_session", map[string]string{"name": fmt.Sprintf("pc-%d", i)}, &got); e != nil {
					t.Fatal(e)
				}
				if got.PID != 1234 || !got.AutomaticDeliveryReady {
					t.Fatal(got)
				}
			}
			for i := 0; i < 2; i++ {
				j := 1 - i
				body := fmt.Sprintf("PID request/reply %d", i)
				if e := call(ctx, locals[i], "send_message", map[string]string{"from": fmt.Sprintf("pc-%d", i), "to": fmt.Sprintf("pc-%d", j), "body": body}, nil); e != nil {
					t.Fatal(e)
				}
				select {
				case m := <-messages[j]:
					if m.Body != body || m.To != fmt.Sprintf("pc-%d", j) {
						t.Fatal(m)
					}
				case <-ctx.Done():
					t.Fatal("automatic local ingress did not receive", ctx.Err())
				}
			}
			isolated := connect("other")
			result, e := isolated.CallTool(ctx, &mcp.CallToolParams{Name: "get_session", Arguments: map[string]string{"name": "pc-0"}})
			if e != nil || !result.IsError {
				t.Fatal("project isolation", result, e)
			}
			cancel()
		})
	}
}
