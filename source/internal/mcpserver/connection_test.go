package mcpserver

import (
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
)

func TestProjectConnectionFollowsListenerAndProject(t *testing.T) {
	registry, err := groups.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	factory := NewRemoteProjectFactory(registry)
	defer factory.Close()
	attachProjectFixture(t, factory, "alpha", "11111111-1111-4111-8111-111111111111", "alpha-worker")
	attachProjectFixture(t, factory, "beta", "22222222-2222-4222-8222-222222222222", "beta-worker")
	server := httptest.NewServer(ProjectHandler(factory))
	defer server.Close()
	for _, project := range []string{"alpha", "beta"} {
		client := connectProjectHTTP(t, server.URL+"/mcp?project="+project)
		sessions := callStructuredTool[[]broker.Session](t, client, "list_sessions", map[string]any{})
		if len(sessions) != 1 || sessions[0].Name != project+"-worker" {
			t.Fatal("listener/project routing changed")
		}
	}
	beta := connectProjectHTTP(t, server.URL+"/mcp?project=beta")
	result, err := beta.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"from": "alpha-worker", "to": "beta-worker", "body": "foreign-sender"}})
	if err != nil || !result.IsError {
		t.Fatal("foreign sender accepted")
	}
}
