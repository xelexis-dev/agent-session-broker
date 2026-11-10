package channel

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestCodexHookOptionalTurnID(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server, shutdown := newIngressServerWithOptions(ctx, &ingressUpstream{}, nil, func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
		return broker.AttachRequest{}, nil
	}, nil, durableOptions{harness: "codex"})
	defer func() { cancel(); shutdown() }()
	cs := catalogClient(t, server)
	for _, tc := range []struct {
		name      string
		extra     map[string]string
		wantError bool
	}{
		{"old config", nil, false},
		{"turn id", map[string]string{"turn_id": "synthetic-turn"}, false},
		{"unknown key", map[string]string{"unknown_key": "synthetic"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]string{"session_id": "not-bound", "hook_event_name": "Stop"}
			for k, v := range tc.extra {
				args[k] = v
			}
			result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "ingress_hook", Arguments: args})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError != tc.wantError {
				t.Fatalf("schema error=%v want=%v", result.IsError, tc.wantError)
			}
		})
	}
}
