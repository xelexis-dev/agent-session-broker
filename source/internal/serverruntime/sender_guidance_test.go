package serverruntime

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
	"github.com/xelexis-dev/agent-session-broker/source/internal/memory"
	"strings"
	"testing"
)

func TestSenderGuidanceIdentityFailure(t *testing.T) {
	store := protectedStore(t)
	remote := broker.NewRemoteService()
	defer remote.Close()
	a, b := protectedRequest(1, "alpha"), protectedRequest(2, "beta")
	protectedRegistered(t, remote, a)
	protectedRegistered(t, remote, b)
	grant, _ := protectedGrant(t, store, "alpha")
	if e := grant.Claim(bindingIdentity(a.Harness, a.SessionID, a.ClientID)); e != nil {
		t.Fatal(e)
	}
	memories, e := memory.NewStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer memories.Close()
	server := mcpserver.New(newProtectedBackend("project", remote), memories)
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			return next(access.WithGrant(ctx, grant), method, req)
		}
	})
	st, ct := mcp.NewInMemoryTransports()
	ss, e := server.Connect(t.Context(), st, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ss.Close()
	cs, e := mcp.NewClient(&mcp.Implementation{Name: "sender-guidance", Version: "0.17.3"}, nil).Connect(t.Context(), ct, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer cs.Close()
	r, e := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]string{"from": "beta", "to": "alpha", "body": "not queued"}})
	if e != nil || !r.IsError {
		t.Fatal("foreign sender accepted")
	}
	text := r.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "identity") || strings.Contains(text, "spawn_session") || strings.Contains(text, "attach_session") {
		t.Errorf("identity failure mislabeled as registration: %s", text)
	}
}
