package mcpserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestRCStatelessSmokeClientMetadata(t *testing.T) {
	for _, serverVersion := range []string{"0.0.0", "0.20.0"} {
		t.Run(serverVersion, func(t *testing.T) {
			policy, _ := protocol.ServerPolicy(serverVersion)
			if policy.MinClient != "0.17.3" || policy.Accepts("0.0.0") || !policy.Accepts("0.17.3") {
				t.Fatal("M3 smoke policy changed")
			}
			b := broker.NewRemoteService()
			defer b.Close()
			server := httptest.NewServer(Handler(newWithCompatibility(b, &stubMemory{}, &policy, serverVersion)))
			defer server.Close()
			for _, version := range []string{"0.0.0", "0.17.3", "0.20.0", "1", ""} {
				meta := map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
				if version != "" {
					meta["io.modelcontextprotocol/clientInfo"] = map[string]string{"name": "agent-session-broker-client", "version": version}
				}
				raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "list_sessions", "arguments": map[string]any{}, "_meta": meta}})
				request, _ := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/mcp", bytes.NewReader(raw))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Accept", "application/json, text/event-stream")
				request.Header.Set("MCP-Protocol-Version", "2026-07-28")
				request.Header.Set("Mcp-Method", "tools/call")
				request.Header.Set("Mcp-Name", "list_sessions")
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal("stateless fixture failed")
				}
				var out struct {
					Result struct {
						IsError bool `json:"isError"`
					}
				}
				err = json.NewDecoder(response.Body).Decode(&out)
				response.Body.Close()
				if err != nil || out.Result.IsError != (version == "0.0.0" || version == "1" || version == "") {
					t.Fatal("RC request client metadata rejected or missing-version admitted")
				}
			}
		})
	}
}
