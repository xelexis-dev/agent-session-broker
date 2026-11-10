package mcpserver

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"testing"
)

func TestAttachmentLegacyCatalog(t *testing.T) {
	s := broker.NewRemoteService()
	defer s.Close()
	c := catalogSession(t, New(s, &stubMemory{}))
	tools, err := c.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("legacy tools/list bytes=%d sha256=%x", len(data), sha256.Sum256(data))
	const baseline = "5aaac0e80d5f9d1140d27ba7b4c41e58d43a92dd4200aa835f7c45b6e77a09fb"
	if fmt.Sprintf("%x", sha256.Sum256(data)) != baseline {
		t.Fatal("legacy tools/list bytes changed")
	}
}

func TestAttachmentCannotInjectArguments(t *testing.T) {
	b := &senderBackend{status: broker.StatusAlive}
	c := catalogSession(t, New(b, &stubMemory{}))
	result, err := c.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"from": "registered", "to": "peer", "body": "hi", "attachment": map[string]any{"id": "invented"}}})
	if err == nil && !result.IsError {
		t.Fatal("arguments injected attachment")
	}
	if b.called {
		t.Fatal("injected attachment reached backend")
	}
}
