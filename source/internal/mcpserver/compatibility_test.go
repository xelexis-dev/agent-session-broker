package mcpserver

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"strings"
	"testing"
)

// The policy input is deliberately separate from DB state and operator flags.

func TestCompatibilityRejectsLegacy(t *testing.T) {
	backend := broker.NewRemoteService()
	defer backend.Close()
	server := newWithCompatibility(backend, &stubMemory{}, &serverCompatibility{MinClient: "0.16.0"}, "0.17.0")
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "agent-session-broker-client", Version: "0.1.0"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	r, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	text := ""
	for _, c := range r.Content {
		if x, ok := c.(*mcp.TextContent); ok {
			text += x.Text
		}
	}
	if !r.IsError || !strings.Contains(text, "client-incompatible") {
		t.Fatalf("legacy call was not rejected: isError=%v", r.IsError)
	}
}

func TestCompatibilityBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, version string
		denied        bool
	}{{"agent-session-broker-client", "0.15.9", true}, {"agent-session-broker-client", "0.16.0", false}, {"agent-session-broker-client", "0.17.0", false}, {"other-client", "0.1.0", true}} {
		t.Run(tc.name+tc.version, func(t *testing.T) {
			backend := broker.NewRemoteService()
			defer backend.Close()
			server := newWithCompatibility(backend, &stubMemory{}, &serverCompatibility{Line: "0.17", MinClient: "0.16.0"}, "0.17.0")
			st, ct := mcp.NewInMemoryTransports()
			ss, e := server.Connect(t.Context(), st, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer ss.Close()
			cs, e := mcp.NewClient(&mcp.Implementation{Name: tc.name, Version: tc.version}, nil).Connect(t.Context(), ct, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer cs.Close()

			for _, name := range []string{"list_sessions", "register_session", "renew_lease", "wait_for_message", "not-a-tool"} {
				if !tc.denied && name != "list_sessions" {
					continue
				}
				r, e := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
				if e != nil || r.IsError != tc.denied {
					t.Fatalf("%s gate: %v", name, e)
				}
			}
		})
	}
}
