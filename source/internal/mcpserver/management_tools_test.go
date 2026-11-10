package mcpserver

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"strings"
	"testing"
)

func TestRemoteManagementToolsAdvertiseUnsupportedBackend(t *testing.T) {
	remote := broker.NewRemoteService()
	defer remote.Close()
	client := durableClient(t, remote)
	listed, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"suspend_session", "unsuspend_session"} {
		found := false
		for _, tool := range listed.Tools {
			if tool.Name == name {
				found = true
				if tool.Description != remoteToolDescriptions[name] {
					t.Fatal("remote management description drift")
				}
			}
		}
		if !found || toolScope(name) != access.SessionManage {
			t.Fatalf("missing remote tool or manage classification: %s", name)
		}
		result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: nameInput{Name: "target"}})
		if err != nil || !result.IsError || len(result.Content) != 1 || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "not supported") {
			t.Fatalf("%s did not return clear unsupported result", name)
		}
	}
}
