package mcpserver

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"strings"
	"testing"
	"time"
)

func recipientServerFixture(t *testing.T) (*broker.RemoteService, broker.Session, broker.Session) {
	r := broker.NewRemoteService()
	t.Cleanup(func() { r.Close() })
	var sessions []broker.Session
	for i, name := range []string{"sender", "receiver"} {
		req := broker.RegisterRequest{Harness: "codex", SessionID: []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"}[i], ClientID: "recipient-client", PID: 100 + i, CWD: "/synthetic", Name: name, ChannelID: name + "-lease", Secret: strings.Repeat("s", 40), RequestID: name + "-request"}
		result, err := r.RegisterSession(t.Context(), req)
		if err != nil || result.Code != "" {
			t.Fatal("recipient fixture")
		}
		sessions = append(sessions, result.Session)
	}
	return r, sessions[0], sessions[1]
}
func TestRecipientMismatchMCPDisclosureByScope(t *testing.T) {
	for _, read := range []bool{false, true} {
		t.Run(map[bool]string{false: "send-only", true: "read-send"}[read], func(t *testing.T) {
			r, from, to := recipientServerFixture(t)
			server := New(r, &stubMemory{})
			scopes := []string{access.MessageSend}
			if read {
				scopes = append(scopes, access.SessionRead)
			}
			server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
				return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
					return next(access.WithGrant(ctx, access.Grant{Scopes: scopes, ExpiresAt: time.Now().Add(time.Hour)}), method, req)
				}
			})
			c := catalogSession(t, server)
			result := callInto[broker.SendResult](t, c, "send_message", map[string]any{"from": from.Name, "to": to.Name, "body": "mismatch", "expectRegistrationId": strings.Repeat("A", 26)})
			if result.Code != protocol.CodeRecipientChanged || result.Accepted || (result.CurrentRegistrationID != "") != read {
				t.Fatal("RPC recipient mismatch disclosure changed")
			}
			if c.InitializeResult().Capabilities.Experimental[protocol.RecipientExpectationCapability] != true {
				t.Fatal("recipient capability missing")
			}
		})
	}
}
func TestRecipientExpectationMCPRejectsNullEmptyAndWrongTypes(t *testing.T) {
	r, from, to := recipientServerFixture(t)
	c := durableClient(t, r)
	for _, bad := range []any{nil, "", "not-registration", 123, []any{}, map[string]any{}} {
		result, err := c.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"from": from.Name, "to": to.Name, "body": "invalid", "expectRegistrationId": bad}})
		if err == nil && !result.IsError {
			raw, _ := json.Marshal(result.StructuredContent)
			var sent broker.SendResult
			_ = json.Unmarshal(raw, &sent)
			if sent.Code != protocol.CodeInvalid || sent.Accepted {
				t.Fatal("invalid expectation became name-only send")
			}
		}
	}
	valid := callInto[broker.SendResult](t, c, "send_message", map[string]any{"from": from.Name, "to": to.Name, "body": "valid", "expectRegistrationId": to.RegistrationID})
	if !valid.Accepted {
		t.Fatal("valid expectation refused")
	}
	got, err := r.WaitForIngress(t.Context(), to.Name, "receiver-lease")
	if err != nil || got.Body != "valid" || got.FromRegistrationID != from.RegistrationID {
		t.Fatal("invalid request enqueued or source attribution missing")
	}
}
