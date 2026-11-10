package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/channel"
)

// Use the wire protocol, including direct calls from clients with a stale
// catalog. Removing discovery alone must not leave the old skill readable.
func TestLegacyRegistrationDiscoveryRemoved(t *testing.T) {
	for _, mode := range []string{"http", "client"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var transport mcp.Transport
			var endpoint string
			if mode == "http" {
				backend := broker.NewRemoteService()
				defer backend.Close()
				server := httptest.NewServer(Handler(New(backend, &stubMemory{})))
				defer server.Close()
				endpoint = server.URL + "/mcp"
				transport = &mcp.StreamableClientTransport{Endpoint: endpoint}
			} else {
				// The local client entry point itself, without a durable registration.
				st, ct := mcp.NewInMemoryTransports()
				done := make(chan struct{})
				go func() { defer close(done); _ = channel.ServeIngress(ctx, st, nil, nil, nil) }()
				defer func() { cancel(); <-done }()
				transport = ct
			}
			conn, err := transport.Connect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			id := 0
			call := func(method, params string) *jsonrpc.Response {
				t.Helper()
				id++
				msg, err := jsonrpc.DecodeMessage([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, id, method, params)))
				if err != nil {
					t.Fatal(err)
				}
				if err := conn.Write(ctx, msg); err != nil {
					t.Fatal(err)
				}
				for {
					msg, err := conn.Read(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if response, ok := msg.(*jsonrpc.Response); ok {
						return response
					}
				}
			}
			init := call("initialize", `{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"discovery-regression","version":"1"}}`)
			if init.Error != nil {
				t.Fatal(init.Error)
			}
			var initialized mcp.InitializeResult
			if err := json.Unmarshal(init.Result, &initialized); err != nil {
				t.Fatal(err)
			}
			if _, ok := initialized.Capabilities.Extensions["io.modelcontextprotocol/skills"]; ok {
				t.Error("obsolete skills extension still advertised")
			}
			for _, method := range []string{"skills/list", "skills/get"} {
				if mode == "http" {
					// On this supported protocol revision the SDK rejects unknown
					// methods at the HTTP boundary, before JSON-RPC dispatch.
					body := fmt.Sprintf(`{"jsonrpc":"2.0","id":99,"method":%q,"params":{"uri":"skill://x-broker/register/SKILL.md"}}`, method)
					req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Accept", "application/json, text/event-stream")
					req.Header.Set("Mcp-Protocol-Version", "2025-03-26")
					response, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					data, err := io.ReadAll(response.Body)
					response.Body.Close()
					if err != nil || response.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), fmt.Sprintf("%q unsupported", method)) {
						t.Errorf("%s: expected unsupported method, got HTTP %d %s (%v)", method, response.StatusCode, data, err)
					}
					continue
				}
				response := call(method, `{"uri":"skill://x-broker/register/SKILL.md"}`)
				var rpcErr *jsonrpc.Error
				if !errors.As(response.Error, &rpcErr) || rpcErr.Code != jsonrpc.CodeMethodNotFound {
					t.Errorf("%s: expected method not found, got %+v", method, response)
				}
			}
			for method, params := range map[string]string{
				"resources/read": `{"uri":"skill://x-broker/register/SKILL.md"}`,
				"prompts/get":    `{"name":"register","arguments":{"name":"unused"}}`,
			} {
				if response := call(method, params); response.Error == nil {
					t.Errorf("%s still serves the obsolete registration instructions", method)
				}
			}
			for _, method := range []string{"resources/list", "prompts/list"} {
				response := call(method, `{}`)
				if response.Error != nil {
					t.Fatalf("%s failed: %v", method, response.Error)
				}
				var catalog struct {
					Resources []json.RawMessage `json:"resources"`
					Prompts   []json.RawMessage `json:"prompts"`
				}
				if err := json.Unmarshal(response.Result, &catalog); err != nil {
					t.Fatal(err)
				}
				if len(catalog.Resources) != 0 || len(catalog.Prompts) != 0 {
					t.Errorf("%s should have no legacy entries: %s", method, response.Result)
				}
			}
			response := call("tools/list", `{}`)
			var listed mcp.ListToolsResult
			if response.Error != nil || json.Unmarshal(response.Result, &listed) != nil {
				t.Fatalf("tool discovery failed: %+v", response)
			}
			foundAttach := false
			for _, tool := range listed.Tools {
				foundAttach = foundAttach || tool.Name == "attach_session"
			}
			if !foundAttach {
				t.Error("explicit recovery tool attach_session was removed")
			}
			if response := call("ping", `{}`); response.Error != nil {
				t.Fatalf("obsolete discovery calls broke the connection: %v", response.Error)
			}
		})
	}
}
