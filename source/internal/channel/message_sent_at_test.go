package channel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestRemoteDeliveryUsesBrokerSentAt(t *testing.T) {
	up := newSwitchableUpstream(t)
	ctx := t.Context()
	for i, name := range []string{"sender", "recipient"} {
		id := broker.AttachRequest{Capabilities: []string{protocol.MessageSentAtCapability}, Harness: "codex", Name: name, SessionID: []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"}[i], PID: 123 + i, CWD: t.TempDir(), ChannelID: name + "-lease"}
		if err := call(ctx, up, "attach_session", id, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, spoof := range []bool{false, true} {
		before := time.Now().UTC().Truncate(time.Millisecond)
		if spoof {
			var message broker.Message
			if err := json.Unmarshal([]byte(`{"from":"sender","to":"recipient","body":"fixture","sentAt":"2000-01-01T00:00:00Z"}`), &message); err != nil {
				t.Fatal(err)
			}
			if _, err := up.service.SendMessage(ctx, message); err != nil {
				t.Fatal(err)
			}
		} else if err := call(ctx, up, "send_message", map[string]string{"from": "sender", "to": "recipient", "body": "fixture"}, nil); err != nil {
			t.Fatal(err)
		}
		after := time.Now().UTC()
		var got struct {
			SentAt time.Time `json:"sentAt"`
			Body   string    `json:"body"`
		}
		if err := call(ctx, up, "wait_for_message", map[string]string{"recipient": "recipient", "channelId": "recipient-lease"}, &got); err != nil {
			t.Fatal(err)
		}
		if got.SentAt.IsZero() || got.SentAt.Before(before) || got.SentAt.After(after) || got.SentAt.Nanosecond()%int(time.Millisecond) != 0 || got.SentAt.Location() != time.UTC {
			t.Fatalf("broker sentAt missing or outside UTC millisecond acceptance window (spoof=%v): %v", spoof, got.SentAt)
		}
	}
}

func TestCodexIngressHookIncludesSentAt(t *testing.T) {
	stamp := time.Date(2026, 10, 18, 1, 2, 3, 456000000, time.UTC)
	r := newCodexIngress(func(_ context.Context, _ broker.Message) error { t.Fatal("unexpected push"); return nil })
	writeCodexHook(r, "PreToolUse")
	if err := r.deliver(t.Context(), broker.Message{From: "a", To: "b", Body: "fixture", SentAt: stamp}); err != nil {
		t.Fatal(err)
	}
	out, err := writeCodexHook(r, "Stop")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out["reason"].(string), `"sentAt":"2026-10-18T01:02:03.456Z"`) {
		t.Fatal("hook dropped sentAt")
	}
}

func TestNewClientOmitsCapabilityForOldServer(t *testing.T) {
	type oldAttach struct {
		Harness   string `json:"harness"`
		Name      string `json:"name"`
		SessionID string `json:"sessionId"`
		PID       int    `json:"pid"`
		CWD       string `json:"cwd"`
		ChannelID string `json:"channelId"`
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "agent-session-broker", Version: "0.17.3"}, nil)
	var attaches int
	mcp.AddTool(server, &mcp.Tool{Name: "attach_session"}, func(_ context.Context, _ *mcp.CallToolRequest, in oldAttach) (*mcp.CallToolResult, broker.Session, error) {
		attaches++
		return nil, broker.Session{ID: in.SessionID, Name: in.Name}, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "wait_for_message"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, broker.Message, error) {
		return nil, broker.Message{From: "a", To: "b", Body: "old"}, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer httpServer.Close()
	up := newTestBrokerUpstream(t.Context(), httpServer.URL)
	defer up.Close()
	var registered broker.Session
	if err := call(t.Context(), up, "attach_session", broker.AttachRequest{Capabilities: []string{protocol.MessageSentAtCapability}, Harness: "codex", Name: "b", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: "/fixture", ChannelID: "lease"}, &registered); err != nil {
		t.Fatal(err)
	}
	var got broker.Message
	if err := call(t.Context(), up, "wait_for_message", struct{}{}, &got); err != nil {
		t.Fatal(err)
	}
	if attaches != 1 || registered.Name != "b" || got.Body != "old" || !got.SentAt.IsZero() {
		t.Fatal("old server compatibility changed")
	}
}

func TestClientDeclaresSentAtOnRegisterAndResume(t *testing.T) {
	up := newSwitchableUpstream(t)
	f := newDurableFixture(t, up, t.TempDir())
	session, result := f.attach(t, "owner")
	if result.IsError || session.RegistrationID == "" {
		t.Fatal("fixture registration failed")
	}
	for _, restart := range []bool{false, true} {
		if restart {
			up.restart()
			waitFor(t, "resumed capable receiver", func() bool { state, err := up.session("owner"); return err == nil && state.AutomaticDeliveryReady })
		}
		before := time.Now().UTC().Truncate(time.Millisecond)
		if err := call(t.Context(), up, "send_message", broker.Message{From: "owner", To: "owner", Body: "fixture"}, nil); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-f.sink:
			if got.SentAt.IsZero() || got.SentAt.Before(before) || got.SentAt.After(time.Now().UTC()) {
				t.Fatal("client did not declare sentAt on the current lease")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("receiver did not deliver fixture")
		}
	}
}
