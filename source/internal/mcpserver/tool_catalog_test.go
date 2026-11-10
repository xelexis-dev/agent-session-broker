package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

func catalogSession(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	st, ct := mcp.NewInMemoryTransports()
	ss, e := server.Connect(t.Context(), st, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, e := mcp.NewClient(&mcp.Implementation{Name: "catalog", Version: "0.17.3"}, nil).Connect(t.Context(), ct, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}
func TestSpawnRemoved(t *testing.T) {
	remote := broker.NewRemoteService()
	defer remote.Close()
	for _, server := range []*mcp.Server{New(remote, &stubMemory{}), New(stubBackend{}, &stubMemory{})} {
		cs := catalogSession(t, server)
		listed, err := cs.ListTools(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range listed.Tools {
			if tool.Name == "spawn_session" {
				t.Error("spawn still listed")
			}
		}
		_, err = cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "spawn_session", Arguments: map[string]any{}})
		if err == nil || !strings.Contains(err.Error(), "unknown tool") {
			t.Fatalf("removed tool should return unknown tool: %v", err)
		}
	}
}

type failedSenderBackend struct {
	*broker.RemoteService
	failure error
}

func (b failedSenderBackend) GetSession(context.Context, string) (broker.Session, error) {
	return broker.Session{}, b.failure
}
func TestRemoteSenderGuidanceSeparatesLookupFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{{"missing", broker.ErrSessionNotFound, "register"}, {"storage", errors.New("raw-secret-path"), "lookup"}} {
		t.Run(tc.name, func(t *testing.T) {
			remote := broker.NewRemoteService()
			defer remote.Close()
			r, e := catalogSession(t, New(failedSenderBackend{remote, tc.err}, &stubMemory{})).CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]string{"from": "fixture", "to": "peer", "body": "hi"}})
			if e != nil || !r.IsError {
				t.Fatal("expected safe tool error")
			}
			text := r.Content[0].(*mcp.TextContent).Text
			if !strings.Contains(text, tc.want) || strings.Contains(text, "spawn_session") || strings.Contains(text, "raw-secret-path") {
				t.Errorf("unsafe or misleading remote guidance: %s", text)
			}
		})
	}
}

func TestRemoteAttachDescriptionPreservesDurableName(t *testing.T) {
	remote := broker.NewRemoteService()
	defer remote.Close()
	listed, err := catalogSession(t, New(remote, &stubMemory{})).ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	const want = "Resuming the same conversation preserves its durable registration and current name. Another conversation cannot claim that name while the durable registration holds it. MCP stop_session keeps the name reserved; detach the registration in the control panel to release it immediately."
	for _, tool := range listed.Tools {
		if tool.Name == "attach_session" {
			if !strings.Contains(tool.Description, want) || strings.Contains(tool.Description, "A retired session may reuse its name") || strings.Contains(tool.Description, "detach the registration to release it") {
				t.Fatalf("attach description does not preserve durable name: %s", tool.Description)
			}
			return
		}
	}
	t.Fatal("attach_session missing")
}
