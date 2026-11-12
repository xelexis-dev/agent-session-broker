package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type panickingListBackend struct{ stubBackend }

func (panickingListBackend) ListSessions(context.Context) ([]broker.Session, error) {
	panic("fixture-tool-panic-secret")
}
func TestMCPToolPanicWholeProcessBoundary(t *testing.T) {
	if os.Getenv("ASB_MCP_PANIC_CHILD") == "1" {
		server := httptest.NewServer(Handler(New(panickingListBackend{}, &stubMemory{})))
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		client := mcp.NewClient(&mcp.Implementation{Name: "panic-boundary", Version: "1.1.0"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
		if err != nil {
			t.Fatal("MCP connect failed")
		}
		defer session.Close()
		_, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
		var rpc *jsonrpc.Error
		if !errors.As(err, &rpc) || rpc.Code != jsonrpc.CodeInternalError || rpc.Message != "Internal error" {
			t.Fatal("MCP panic did not become fixed internal error")
		}
		if _, err = session.ListTools(ctx, nil); err != nil {
			t.Fatal("MCP process/session did not survive panic")
		}
		serverdiag.Shutdown(ctx)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMCPToolPanicWholeProcessBoundary$", "-test.count=1")
	cmd.Env = append(os.Environ(), "ASB_MCP_PANIC_CHILD=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal("MCP panic child crashed or failed fixed-error check")
	}
	if bytes.Contains(output, []byte("fixture-tool-panic-secret")) || !strings.Contains(string(output), "server_request_panic") {
		t.Fatal("MCP panic value exposed or sanitized diagnostic absent")
	}
}
