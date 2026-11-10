package channel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpconfig"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func updateHintStatus(t *testing.T, current, server string, recommended any, host string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithCancel(mcpconfig.WithVersion(t.Context(), current))
	t.Cleanup(cancel)
	experimental := map[string]any{}
	if recommended != nil {
		experimental["recommendedClientVersion"] = recommended
	}
	remote := mcp.NewServer(&mcp.Implementation{Name: "test", Version: server}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{Experimental: experimental}})
	central := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, nil))
	t.Cleanup(central.Close)
	upstream := newTestBrokerUpstream(ctx, central.URL)
	t.Cleanup(func() { upstream.Close() })
	if _, err := upstream.session(ctx); err != nil {
		t.Fatal(err)
	}
	local, shutdown := newIngressServerWithOptions(ctx, upstream, nil, func(context.Context, *mcp.CallToolRequest) (protocol.AttachRequest, error) {
		return protocol.AttachRequest{Harness: host, SessionID: "synthetic-thread"}, nil
	}, nil, durableOptions{tokenMode: true, harness: host})
	t.Cleanup(func() { cancel(); shutdown() })
	st, ct := mcp.NewInMemoryTransports()
	ss, err := local.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	client, err := mcp.NewClient(&mcp.Implementation{Name: "host", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "auth_status", Arguments: struct{}{}})
	if err != nil || result.IsError {
		t.Fatal("auth status failed", err)
	}
	data, _ := json.Marshal(result.StructuredContent)
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestAuthStatusRecommendedClientUpdate(t *testing.T) {
	status := updateHintStatus(t, "0.17.2", "0.17.3", "0.17.3", "claude")
	update, ok := status["update"].(map[string]any)
	if !ok || update["available"] != true || update["current"] != "0.17.2" || update["recommended"] != "0.17.3" {
		t.Fatal("auth_status missing recommended client update", status["update"])
	}
}

func TestAuthStatusUpdateUsesRecommendedSemverOnly(t *testing.T) {
	for _, tc := range []struct {
		name, current, server string
		recommended           any
		want                  bool
	}{
		{"missing", "0.17.2", "0.19.9", nil, false},
		{"equal", "0.17.3", "0.19.9", "0.17.3", false},
		{"newer-client", "0.17.4", "0.19.9", "0.17.3", false},
		{"server-only-release", "0.17.2", "0.19.9", "0.17.2", false},
		{"numeric", "0.9.9", "0.19.9", "0.17.3", true},
		{"prerelease", "0.17.3-rc.1", "0.19.9", "0.17.3", true},
		{"prerelease-order", "0.17.3-rc.2", "0.19.9", "0.17.3-rc.10", true},
		{"build-metadata", "0.17.3+local", "0.19.9", "0.17.3+server", false},
		{"dev-client", "dev", "0.19.9", "0.17.3", false},
		{"invalid-current", "01.17.2", "0.19.9", "0.17.3", false},
		{"invalid-recommendation", "0.17.2", "0.19.9", "0.17.3; synthetic-untrusted-command", false},
		{"wrong-type", "0.17.2", "0.19.9", 42, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := updateHintStatus(t, tc.current, tc.server, tc.recommended, "codex")
			update, present := status["update"]
			if present != tc.want {
				t.Fatal("wrong update presence", update)
			}
			if tc.want {
				hint := update.(map[string]any)
				steps := hint["steps"].(string)
				if !strings.Contains(steps, "original host mode and flags") || strings.Contains(steps, "\n") {
					t.Fatal("unsafe host guidance", steps)
				}
			}
		})
	}
}
func TestUpdateStepsFixedForEachHostAndWindows(t *testing.T) {
	for host, fragment := range map[string]string{"claude": "/mcp > x-broker > Reconnect", "codex": "daemon-based TUI alone does not reload MCP", "kimi": "unmeasured", "": "original host mode and flags"} {
		for _, platform := range []string{"darwin", "windows"} {
			steps := updateSteps(host, platform)
			if !strings.Contains(steps, fragment) || strings.Contains(steps, "\n") {
				t.Fatal("wrong fixed steps", steps)
			}
			if strings.Contains(steps, "Windows:") != (platform == "windows") {
				t.Fatal("wrong platform note")
			}
		}
	}
}
