package channel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestAttachmentWithoutBoundSourceSendsNothing(t *testing.T) {
	for _, path := range []string{"../outside", ".EnV", ".CrEdEnTiAlS/key", "/absolute/file", "link/payload"} {
		t.Run(path, func(t *testing.T) {
			up := &fakeUpstream{}
			out, err := callWithRecipientExpectation(t.Context(), up, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"from": "sender", "to": "recipient", "body": "body", "attachment": path}})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(out.StructuredContent)
			var result broker.SendResult
			if json.Unmarshal(raw, &result) != nil || result.Code != "unsupported" {
				t.Fatal("unbound attachment was not refused with a fixed code")
			}
			if len(up.calls) != 0 {
				t.Fatal("blocked attachment reached upstream")
			}
		})
	}
}

func TestAttachmentClaudeMetadataProjection(t *testing.T) {
	meta := messageMetadata(broker.Message{Attachment: &broker.Attachment{ID: "private-id", Name: "private-name", Path: "/inbox/file", Size: 12, SHA256: "digest"}})
	if meta["attachment_path"] != "/inbox/file" || meta["attachment_size"] != "12" || meta["attachment_sha256"] != "digest" {
		t.Fatal("Claude fallback lost local attachment")
	}
	raw, _ := json.Marshal(meta)
	var decoded map[string]string
	_ = json.Unmarshal(raw, &decoded)
	if decoded["attachment_id"] != "" || decoded["attachment_name"] != "" {
		t.Fatal("server reference escaped")
	}
	meta = messageMetadata(broker.Message{Attachment: &broker.Attachment{Error: "integrity"}})
	if meta["attachment_error"] != "integrity" || meta["attachment_path"] != "" {
		t.Fatal("Claude fallback lost attachment error")
	}
}

func TestAttachmentCodexHookAndSteerProjection(t *testing.T) {
	m := broker.Message{Body: "synthetic", Attachment: &broker.Attachment{ID: "hidden-id", Name: "hidden-name", Path: "/inbox/file", Size: 7, SHA256: "digest"}}
	router := newCodexIngress(func(context.Context, broker.Message) error { t.Fatal("unexpected push"); return nil })
	writeCodexHook(router, "PreToolUse")
	if router.deliver(t.Context(), m) != nil {
		t.Fatal("hook delivery")
	}
	out, e := writeCodexHook(router, "Stop")
	if e != nil {
		t.Fatal(e)
	}
	hook := out["reason"].(string)
	steer, e := codexMessageContent([]broker.Message{m})
	if e != nil {
		t.Fatal(e)
	}
	for _, text := range []string{hook, steer} {
		if strings.Contains(text, "hidden-id") || strings.Contains(text, "hidden-name") || !strings.Contains(text, "/inbox/file") || !strings.Contains(text, broker.AttachmentReminder) {
			t.Fatal("hook/steer projection unsafe")
		}
	}
}
func TestAttachmentKimiRecoveryProjection(t *testing.T) {
	r := &kimiIngress{}
	m := broker.Message{Body: "synthetic", Attachment: &broker.Attachment{ID: "hidden-id", Path: "/inbox/file", Size: 7, SHA256: "digest"}}
	if e := r.deliver(t.Context(), m); e != nil {
		t.Fatal(e)
	}
	out, _ := r.hook("Stop")
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "hidden-id") || !strings.Contains(string(raw), "/inbox/file") {
		t.Fatal("Kimi projection lost attachment")
	}
}
