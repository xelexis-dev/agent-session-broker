package channel

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOldServerExplicitRecipientExpectationIsNeverSentOrStripped(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "agent-session-broker", Version: "0.17.3"}, nil)
	var calls atomic.Int32
	type oldInput struct {
		From string `json:"from"`
		To   string `json:"to"`
		Body string `json:"body"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "send_message"}, func(context.Context, *mcp.CallToolRequest, oldInput) (*mcp.CallToolResult, broker.SendResult, error) {
		calls.Add(1)
		return nil, broker.SendResult{Accepted: true}, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer httpServer.Close()
	u := newTestBrokerUpstream(t.Context(), httpServer.URL)
	defer u.Close()
	for _, direct := range []bool{false, true} {
		p := &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"from": "a", "to": "b", "body": "pinned", "expectRegistrationId": strings.Repeat("A", 26)}}
		var result *mcp.CallToolResult
		var err error
		if direct {
			result, err = u.CallTool(t.Context(), p)
		} else {
			result, err = callWithRecipientExpectation(t.Context(), u, p)
		}
		if err != nil {
			t.Fatal("unsupported send transport failed")
		}
		raw, _ := json.Marshal(result.StructuredContent)
		var sent broker.SendResult
		_ = json.Unmarshal(raw, &sent)
		if sent.Code != broker.CodeUnsupported || sent.Accepted || calls.Load() != 0 {
			t.Fatal("old server received or stripped explicit expectation")
		}
	}
	var old broker.SendResult
	if err := call(t.Context(), u, "send_message", map[string]string{"from": "a", "to": "b", "body": "name-only"}, &old); err != nil || !old.Accepted || calls.Load() != 1 {
		t.Fatal("old name-only behavior changed")
	}
}
func TestClientForwardsRecipientExpectationAndSourceToNativeSink(t *testing.T) {
	r := newDetachedRuntime(t)
	a := r.harness(t, t.TempDir(), 2991)
	b := r.harness(t, t.TempDir(), 2992)
	a.attach(t)
	b.attach(t)
	result, err := a.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"from": a.session.Name, "to": b.session.Name, "body": "pinned", "expectRegistrationId": b.session.RegistrationID}})
	if err != nil || result.IsError {
		t.Fatal("client expectation forwarding failed")
	}
	raw, _ := json.Marshal(result.StructuredContent)
	var sent broker.SendResult
	_ = json.Unmarshal(raw, &sent)
	if !sent.Accepted {
		t.Fatal("client stripped or rejected supported expectation")
	}
	select {
	case got := <-b.received:
		if got.FromRegistrationID != a.session.RegistrationID || got.RegistrationID != b.session.RegistrationID {
			t.Fatal("client sink lost source or recipient identity")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pinned delivery missing")
	}
}
func TestChannelAndCodexHookPreserveSourceRegistrationAndSize(t *testing.T) {
	id := strings.Repeat("A", 26)
	m := broker.Message{From: "from", To: "to", Body: "frame", FromRegistrationID: id}
	if messageMetadata(m)["fromRegistrationId"] != id {
		t.Fatal("Claude event metadata dropped source")
	}
	router := newCodexIngress(func(context.Context, broker.Message) error { t.Fatal("unexpected direct push"); return nil })
	writeCodexHook(router, "PreToolUse")
	if router.deliver(t.Context(), m) != nil {
		t.Fatal("Codex hook delivery")
	}
	result, err := writeCodexHook(router, "Stop")
	if err != nil || !strings.Contains(result["reason"].(string), `"fromRegistrationId":"`+id+`"`) {
		t.Fatal("Codex hook dropped source")
	}
	raw, _ := json.Marshal(m)
	m.Body = strings.Repeat("x", 12000-len(raw)+len(m.Body))
	writeCodexHook(router, "PreToolUse")
	if router.deliver(t.Context(), m) != nil {
		t.Fatal("exact Codex envelope rejected")
	}
	m.Body += "x"
	if router.deliver(t.Context(), m) == nil {
		t.Fatal("Codex source metadata bypassed envelope size")
	}
}
