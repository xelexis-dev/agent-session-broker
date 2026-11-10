package mcpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

type emptyListBackend struct{ stubBackend }

func (emptyListBackend) ListSessions(context.Context) ([]broker.Session, error) { return nil, nil }

func TestHTTPListSessionsSupportsObjectOnlyMCPClients(t *testing.T) {
	for _, test := range []struct {
		name    string
		backend Backend
		count   int
	}{
		{"populated", stubBackend{}, 1}, {"empty", emptyListBackend{}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(Handler(New(test.backend, &stubMemory{})))
			defer server.Close()
			ctx := context.Background()
			client := mcp.NewClient(&mcp.Implementation{Name: "compatibility-test", Version: "0.17.3"}, nil)
			cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()
			result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
			if err != nil || result.IsError {
				t.Fatalf("list = %+v, %v", result, err)
			}
			if result.StructuredContent != nil {
				t.Fatalf("object-only clients cannot use array structured output: %#v", result.StructuredContent)
			}
			if len(result.Content) != 1 {
				t.Fatalf("content = %+v", result.Content)
			}
			content, ok := result.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("not text: %T", result.Content[0])
			}
			var sessions []broker.Session
			if err := json.Unmarshal([]byte(content.Text), &sessions); err != nil || sessions == nil || len(sessions) != test.count {
				t.Fatalf("list text = %q, %v", content.Text, err)
			}
			listed, err := cs.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, tool := range listed.Tools {
				if tool.Name == "list_sessions" && tool.OutputSchema != nil {
					t.Fatalf("text-only tool advertises output schema: %+v", tool.OutputSchema)
				}
			}
		})
	}
}
