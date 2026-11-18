package channel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientdelivery"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestIncomingProjectUsesBoundConnection(t *testing.T) {
	for _, project := range []string{"P1", "P2"} {
		t.Run(project, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			up := &ingressUpstream{input: make(chan broker.Message)}
			id := broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: t.TempDir()}
			router := newCodexIngress(func(context.Context, broker.Message) error { t.Error("unexpected external delivery"); return nil })
			server, shutdown := newIngressServerWithOptions(ctx, up, nil,
				func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) { return id, nil },
				func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
					return router.deliver, router, nil
				}, durableOptions{project: project})
			defer func() { cancel(); shutdown() }()
			st, ct := mcp.NewInMemoryTransports()
			ss, err := server.Connect(ctx, st, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ss.Close()
			client, err := mcp.NewClient(&mcp.Implementation{Name: "fixture", Version: "1"}, nil).Connect(ctx, ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]string{"name": "receiver"}})
			if err != nil || result.IsError {
				t.Fatal("fixture attach failed")
			}
			writeCodexHook(router, "PreToolUse")
			other := "P2"
			if project == "P2" {
				other = "P1"
			}
			message := broker.Message{From: "pm", To: "receiver", DeliverySequence: 7, Body: `{"project":"` + other + `"}`}
			if err := router.deliver(ctx, message); err != nil {
				t.Fatal(err)
			}
			result, err = client.CallTool(ctx, &mcp.CallToolParams{Name: "ingress_hook", Meta: mcp.Meta{"threadId": id.SessionID}, Arguments: map[string]string{"session_id": id.SessionID, "hook_event_name": "Stop"}})
			if err != nil || result.IsError {
				t.Fatal("fixture hook failed")
			}
			raw, _ := json.Marshal(result.StructuredContent)
			var output struct {
				Reason string `json:"reason"`
			}
			if json.Unmarshal(raw, &output) != nil {
				t.Fatal("invalid hook output")
			}
			start := strings.Index(output.Reason, "[")
			if start < 0 {
				t.Fatal("missing hook envelope")
			}
			var rows []map[string]any
			if json.Unmarshal([]byte(output.Reason[start:]), &rows) != nil || len(rows) != 1 {
				t.Fatal("invalid hook envelope")
			}
			if rows[0]["project"] != project {
				t.Fatalf("missing or wrong bound project: got %v want %s", rows[0]["project"], project)
			}
			if rows[0]["body"] != message.Body || rows[0]["from"] != "pm" || rows[0]["deliverySequence"] != float64(7) {
				t.Fatal("message payload or identity changed")
			}
		})
	}
}

func TestBoundProjectFinalProjections(t *testing.T) {
	for _, project := range []string{"P1", "P2"} {
		t.Run(project, func(t *testing.T) {
			message := broker.Message{From: "pm", To: "receiver", DeliverySequence: 7, Body: `{"project":"spoof"}`, Attachment: &broker.Attachment{ID: "private-id", Name: "private-name", Path: "/fixture/file", Size: 3, SHA256: strings.Repeat("a", 64)}}
			assertRows := func(text string) {
				t.Helper()
				start := strings.Index(text, "[")
				var rows []map[string]any
				if start < 0 || json.Unmarshal([]byte(text[start:]), &rows) != nil || len(rows) != 1 {
					t.Fatal("missing final envelope")
				}
				if rows[0]["project"] != project || rows[0]["from"] != "pm" || rows[0]["deliverySequence"] != float64(7) || !strings.HasPrefix(rows[0]["body"].(string), message.Body) {
					t.Fatal("bound identity/payload changed")
				}
				if strings.Contains(text, "private-id") || strings.Contains(text, "private-name") {
					t.Fatal("attachment authority leaked")
				}
			}
			var sinkProject, pushProject string
			router := newCodexIngress(func(ctx context.Context, _ broker.Message) error {
				pushProject = clientdelivery.Project(ctx)
				return nil
			})
			factory := boundIngressFactory(project, func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
				return func(ctx context.Context, m broker.Message) error {
					sinkProject = clientdelivery.Project(ctx)
					meta := messageMetadata(m, sinkProject)
					if meta["project"] != project || meta["from"] != "pm" || meta["deliverySequence"] != "7" || meta["attachment_path"] != "/fixture/file" {
						t.Fatal("channel metadata changed")
					}
					return nil
				}, router, nil
			})
			sink, _, err := factory(broker.AttachRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if err = sink(t.Context(), message); err != nil {
				t.Fatal(err)
			}
			if err = router.push(t.Context(), message); err != nil {
				t.Fatal(err)
			}
			if sinkProject != project || pushProject != project {
				t.Fatal("binding context lost, including delayed push")
			}
			// Hook and steer share this final projection; the separate core test uses the actual hook stack.
			text, err := codexMessageContent([]broker.Message{message}, router.project)
			if err != nil {
				t.Fatal(err)
			}
			assertRows(text)
			queue := &kimiIngress{project: project}
			if err = queue.deliver(t.Context(), message); err != nil {
				t.Fatal(err)
			}
			out, drained, batch := queue.hookHandoff("UserPromptSubmit", true)
			if !drained {
				t.Fatal("Kimi prompt not delivered")
			}
			assertRows(out["message"].(string))
			// Retry the same handed-off batch without changing its binding or identity.
			if batch == nil {
				batch = &kimiBatch{messages: []broker.Message{message}, generation: queue.generation}
			}
			queue.retry(batch)
			out, drained, _ = queue.hookHandoff("Stop", true)
			if !drained {
				t.Fatal("Kimi retry not delivered")
			}
			assertRows(out["hookSpecificOutput"].(map[string]any)["permissionDecisionReason"].(string))
			_, _, err = boundIngressFactory("other", func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
				return sink, router, nil
			})(broker.AttachRequest{})
			if err == nil {
				t.Fatal("cross-project router reused")
			}
		})
	}
}

func TestEmptyBoundProjectOmitted(t *testing.T) {
	message := broker.Message{From: "pm", To: "receiver", Body: "fixture"}
	if _, present := messageMetadata(message, "")["project"]; present {
		t.Error("empty bound project emitted in channel metadata")
	}
	text, err := codexMessageContent([]broker.Message{message}, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, `"project"`) {
		t.Error("empty bound project emitted in Codex output")
	}
	queue := &kimiIngress{}
	if err = queue.deliver(t.Context(), message); err != nil {
		t.Fatal(err)
	}
	output, _, _ := queue.hookHandoff("UserPromptSubmit", true)
	if strings.Contains(output["message"].(string), `"project"`) {
		t.Error("empty bound project emitted in Kimi output")
	}
	raw, err := json.Marshal(clientdelivery.Entry{DeliveredMessage: broker.DeliveredMessage{Body: "fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"project"`) {
		t.Error("empty bound project emitted in native/external entry")
	}
}
