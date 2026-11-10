package channel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpconfig"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestMessageAckNeverSendsFieldsToOldServer(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "old", Version: "0.17.3"}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{Experimental: map[string]any{broker.MessageSentAtCapability: true, broker.ReceiverHolderCapability: true}}})
	var calls atomic.Int32
	for _, name := range []string{"register_session", "resume_session", "renew_lease", "wait_for_message"} {
		properties := map[string]any{}
		switch name {
		case "register_session", "resume_session":
			properties["capabilities"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
		case "renew_lease":
			properties["registrationId"] = map[string]any{"type": "string"}
			properties["channelId"] = map[string]any{"type": "string"}
		case "wait_for_message":
			properties["recipient"] = map[string]any{"type": "string"}
			properties["channelId"] = map[string]any{"type": "string"}
		}
		server.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object", "properties": properties, "additionalProperties": false}}, func(_ context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args map[string]any
			if json.Unmarshal(r.Params.Arguments, &args) != nil {
				t.Error("invalid old-server request")
			}
			for _, field := range []string{"epoch", "cursor", "ackThrough"} {
				if _, found := args[field]; found {
					t.Error("ack field reached old server", field)
				}
			}
			if raw, ok := args["capabilities"].([]any); ok {
				for _, capability := range raw {
					if capability == broker.MessageAckCapability {
						t.Error("ack negotiated without server advertisement")
					}
				}
			}
			calls.Add(1)
			return &mcp.CallToolResult{StructuredContent: map[string]any{"ok": true}}, nil
		})
	}
	central := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer central.Close()
	ctx := mcpconfig.WithVersion(t.Context(), "1.0.0")
	up := newTestBrokerUpstream(ctx, central.URL)
	defer up.Close()
	if up.verifyMessageAck(ctx) {
		t.Fatal("old server advertised acknowledgement")
	}
	inputs := map[string]any{
		"register_session": map[string]any{"capabilities": []string{broker.MessageSentAtCapability, broker.MessageAckCapability}},
		"resume_session":   map[string]any{"capabilities": []string{broker.MessageSentAtCapability, broker.MessageAckCapability}},
		"renew_lease":      broker.RenewRequest{RegistrationID: "registration", ChannelID: "lease", Epoch: "epoch", AckThrough: 9},
		"wait_for_message": broker.IngressRequest{Recipient: "receiver", ChannelID: "lease", RegistrationID: "registration", Epoch: "epoch", AckThrough: 9, Cursor: 9},
	}
	for name, input := range inputs {
		result, err := up.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: input})
		if err != nil || result.IsError {
			t.Fatal("old schema rejected stripped request", name, err)
		}
	}
	if calls.Load() != 4 {
		t.Fatal("old-server calls did not execute")
	}
}
