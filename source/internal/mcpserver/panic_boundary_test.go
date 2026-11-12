package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/accesslog"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
		handler := Handler(New(panickingListBackend{}, &stubMemory{}))
		var logger *accesslog.Logger
		var logPath string
		if os.Getenv("ASB_MCP_PANIC_ACCESS") == "on" {
			root := t.TempDir()
			cfg := accesslog.Defaults()
			cfg.File = "logs/access.log"
			var err error
			logger, err = accesslog.Open(root, cfg)
			if err != nil {
				t.Fatal("access log fixture failed")
			}
			serverdiag.RegisterRoutes("/mcp")
			logPath = filepath.Join(root, "logs", "access.log")
			handler = logger.Handler(handler, func(*http.Request) accesslog.Metadata {
				return accesslog.Metadata{Route: "/mcp", Address: accesslog.Address{Peer: "127.0.0.1", Client: "127.0.0.1", Source: "peer"}}
			})
		}
		server := httptest.NewServer(handler)
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
		_ = session.Close()
		server.Close()
		if logger != nil {
			drain, stop := context.WithTimeout(context.Background(), 2*time.Second)
			defer stop()
			logger.Close(drain)
			raw, err := os.ReadFile(logPath)
			if err != nil || bytes.Contains(raw, []byte("fixture-tool-panic-secret")) {
				t.Fatal("access file missing or exposed panic value")
			}
			panics := 0
			for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
				var event accesslog.Record
				if json.Unmarshal(line, &event) != nil {
					t.Fatal("malformed access row")
				}
				if event.Type == "access" && event.Panic && event.Route == "/mcp" {
					panics++
				}
			}
			if panics != 1 {
				t.Fatal("handled MCP panic did not yield exactly one access row")
			}
		}
		drain, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		serverdiag.Shutdown(drain)
		return
	}
	for _, mode := range []string{"off", "on"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestMCPToolPanicWholeProcessBoundary$", "-test.count=1")
			cmd.Env = append(os.Environ(), "ASB_MCP_PANIC_CHILD=1", "ASB_MCP_PANIC_ACCESS="+mode)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatal("MCP panic child crashed or failed fixed-error/access check")
			}
			if bytes.Contains(output, []byte("fixture-tool-panic-secret")) || !strings.Contains(string(output), "server_request_panic") {
				t.Fatal("MCP panic value exposed or sanitized diagnostic absent")
			}
		})
	}
}
