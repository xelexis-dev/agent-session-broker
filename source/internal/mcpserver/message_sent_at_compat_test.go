package mcpserver

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// Schemas extracted from each released tag's embedded channel/tooldefs.json.
// 0.16 removed wait_for_message from its model catalog; its internal Go receiver
// still decodes Message and ignores unknown optional fields.
func TestMessageSentAtCompatibilityWithReleasedSchemas(t *testing.T) {
	raw, err := os.ReadFile("testdata/message-output-schemas.json")
	if err != nil {
		t.Fatal(err)
	}
	var versions map[string]map[string]*jsonschema.Schema
	if err := json.Unmarshal(raw, &versions); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"0.12.0", "0.13.0", "0.14.0", "0.15.0", "0.16.0"} {
		t.Run(version, func(t *testing.T) {
			service := broker.NewRemoteService()
			t.Cleanup(func() { service.Close() })
			id := broker.AttachRequest{Harness: "codex", Name: "fixture", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: "/fixture", ChannelID: "old-lease"}
			if _, err := service.AttachSession(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			client := catalogSession(t, New(service, &stubMemory{}))
			for _, notice := range []bool{false, true} {
				if notice {
					if _, err := service.SendAnnouncement(t.Context(), id.Name, "notice"); err != nil {
						t.Fatal(err)
					}
				} else {
					result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]string{"from": id.Name, "to": id.Name, "body": "fixture"}})
					if err != nil || result.IsError {
						t.Fatal("send failed")
					}
					validateReleasedMessageSchema(t, versions[version]["send_message"], result.StructuredContent)
				}
				result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "wait_for_message", Arguments: map[string]string{"recipient": id.Name, "channelId": id.ChannelID}})
				if err != nil || result.IsError {
					t.Fatal("wait failed")
				}
				bytes, _ := json.Marshal(result.StructuredContent)
				var value map[string]any
				json.Unmarshal(bytes, &value)
				if _, found := value["sentAt"]; found {
					t.Fatal("undeclared recipient got sentAt")
				}
				if schema := versions[version]["wait_for_message"]; schema != nil {
					validateReleasedMessageSchema(t, schema, value)
				} else if version != "0.16.0" {
					t.Fatal("missing released wait schema")
				}
			}
			if schema := versions[version]["wait_for_message"]; schema != nil {
				stamp := time.Date(2026, 10, 18, 1, 2, 3, 456000000, time.UTC)
				batch := protocol.Message{Batch: &protocol.MessageBatch{Messages: []protocol.DeliveredMessage{{From: "a", To: "b", Body: "one", SentAt: stamp}, {From: "c", To: "b", Body: "two", SentAt: stamp.Add(time.Second)}}}}
				validateReleasedMessageSchema(t, schema, protocol.ProjectMessage(batch, false))
			}
			id.Capabilities = []string{protocol.MessageSentAtCapability}
			// A new attach/lease is the only way to opt into delivery metadata.
			if _, err := service.AttachSession(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			if _, err := service.SendMessage(t.Context(), broker.Message{From: id.Name, To: id.Name, Body: "new"}); err != nil {
				t.Fatal(err)
			}
			result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "wait_for_message", Arguments: map[string]string{"recipient": id.Name, "channelId": id.ChannelID}})
			if err != nil || result.IsError {
				t.Fatal("declared wait failed")
			}
			data, _ := json.Marshal(result.StructuredContent)
			var got broker.Message
			json.Unmarshal(data, &got)
			if got.SentAt.IsZero() {
				t.Fatal("declared recipient lost sentAt")
			}
		})
	}
}

func validateReleasedMessageSchema(t *testing.T, schema *jsonschema.Schema, value any) {
	t.Helper()
	if schema == nil {
		t.Fatal("missing schema")
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	json.Unmarshal(raw, &decoded)
	if err := resolved.Validate(decoded); err != nil {
		t.Fatal(err)
	}
}
