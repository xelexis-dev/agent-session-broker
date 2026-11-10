package mcpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

// Copied from 0.17.2's CompatibilityGuidance template; old clients regenerate
// and compare this complete string before accepting the minimum version.
const oldClientRefusal = "client-incompatible: client 0.17.2; server 1.0.0; required minimum 0.17.3. Run npm install -g @xelexis/x-broker@latest, then reconnect (Claude: /mcp; Codex: restart the app)."

func TestMinimumClientStatelessLaterCalls(t *testing.T) {
	b := broker.NewRemoteService()
	defer b.Close()
	server := httptest.NewServer(Handler(newWithCompatibility(b, &stubMemory{}, &serverCompatibility{MinClient: "0.17.3"}, "1.0.0")))
	defer server.Close()
	call := func(t *testing.T, method string, params map[string]any) map[string]any {
		t.Helper()
		meta, _ := params["_meta"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
			params["_meta"] = meta
		}
		meta["io.modelcontextprotocol/protocolVersion"] = "2026-07-28"
		meta["io.modelcontextprotocol/clientCapabilities"] = map[string]any{}
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		req, _ := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/mcp", bytes.NewReader(data))
		req.Header.Set("Mcp-Method", method)
		if name, ok := params["name"].(string); ok {
			req.Header.Set("Mcp-Name", name)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2026-07-28")
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		var envelope map[string]any
		if json.Unmarshal(body, &envelope) != nil {
			t.Fatal("invalid MCP response")
		}
		result, ok := envelope["result"].(map[string]any)
		if !ok {
			t.Fatalf("MCP result missing: %s", body)
		}
		return result
	}
	meta := func(name, version string) map[string]any {
		return map[string]any{"io.modelcontextprotocol/clientInfo": map[string]any{"name": name, "version": version}}
	}
	// A successful discover/list cannot authorize later stateless calls.
	call(t, "server/discover", map[string]any{"_meta": meta("agent-session-broker-client", "0.17.3")})
	call(t, "tools/list", map[string]any{"_meta": meta("agent-session-broker-client", "0.17.3")})
	for _, tc := range []struct {
		name, version string
		deny          bool
	}{
		{"agent-session-broker-client", "0.17.2", true}, {"other-client", "0.17.2", true}, {"agent-session-broker-client", "0.17.3", false}, {"agent-session-broker-client", "0.17.3-rc.1", true}, {"agent-session-broker-client", "0.17.3+build", false}, {"other-client", "bad", true}, {"", "", true},
	} {
		t.Run(tc.name+tc.version, func(t *testing.T) {
			for _, name := range []string{"list_sessions", "register_session", "resume_session", "renew_lease", "send_message", "memory_put", "not-a-tool"} {
				if !tc.deny && name != "list_sessions" {
					continue
				}
				params := map[string]any{"name": name, "arguments": map[string]any{}}
				if tc.version != "" {
					params["_meta"] = meta(tc.name, tc.version)
				}
				r := call(t, "tools/call", params)
				denied, _ := r["isError"].(bool)
				if denied != tc.deny {
					t.Fatalf("%s denied=%v want=%v", name, denied, tc.deny)
				}
				if tc.version == "0.17.2" {
					content := r["content"].([]any)
					if content[0].(map[string]any)["text"] != oldClientRefusal {
						t.Fatal("old client refusal bytes changed")
					}
				}
			}
		})
	}
}
