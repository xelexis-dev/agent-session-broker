package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestActivityProtocolSchemaAndHeartbeatCompatibility(t *testing.T) {
	service := broker.NewRemoteService()
	defer service.Close()
	client := durableClient(t, service)
	register := broker.RegisterRequest{Holder: g4FixtureHolder(), Harness: "claude", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: "/work", ClientID: "client-activity", Name: "activity", ChannelID: "activity-lease", Secret: strings.Repeat("s", 40), RequestID: "activity-register"}
	registered := callInto[broker.RegisterResult](t, client, "register_session", register)
	if registered.ActivityVersion != protocol.ActivityVersion || registered.Session.Activity.State != "unknown" {
		t.Fatal("server did not advertise activity v1")
	}
	tools, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range tools.Tools {
		if tool.Name == "renew_lease" {
			found = true
			raw, _ := json.Marshal(tool.InputSchema)
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			}
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatal(err)
			}
			activity, exists := schema.Properties["activity"]
			if !exists || string(activity) != "{}" {
				t.Fatalf("activity must be permissive raw JSON, got %s", activity)
			}
			for _, name := range schema.Required {
				if name == "activity" {
					t.Fatal("activity became mandatory")
				}
			}
		}
	}
	if !found {
		t.Fatal("renew schema missing")
	}
	for _, activity := range []any{"untrusted-error-marker", 17, true, []any{}, map[string]any{}, map[string]any{"state": "bad", "source": "untrusted-error-marker"}, nil} {
		result := callInto[broker.RegisterResult](t, client, "renew_lease", map[string]any{"registrationId": registered.Session.RegistrationID, "channelId": register.ChannelID, "activity": activity})
		if result.Code != "" || result.Session.Status != broker.StatusAlive || result.Session.Activity.State != "unknown" {
			t.Fatal("optional malformed activity rejected heartbeat")
		}
	}
	sample := protocol.Activity{State: protocol.ActivityWorking, Source: "claude-session-file", Sequence: 1, ObservedAt: time.Now().UTC()}
	renewed := callInto[broker.RegisterResult](t, client, "renew_lease", map[string]any{"registrationId": registered.Session.RegistrationID, "channelId": register.ChannelID, "activity": sample})
	if renewed.ActivityVersion != 1 || renewed.Session.Activity.State != "working" {
		t.Fatal("valid MCP activity was not applied")
	}
	got := callInto[broker.Session](t, client, "get_session", map[string]any{"name": register.Name})
	list := callInto[[]broker.Session](t, client, "list_sessions", map[string]any{})
	if got.Activity != nil || len(list) != 1 || list[0].Activity.State != "working" {
		t.Fatal("anonymous get must omit activity; list must keep it")
	}
	// An old client still renews with exactly its original fields.
	old := callInto[broker.RegisterResult](t, client, "renew_lease", map[string]any{"registrationId": registered.Session.RegistrationID, "channelId": register.ChannelID})
	if old.Code != "" || old.Session.Activity.State != "unknown" || old.Session.Status != broker.StatusAlive {
		t.Fatal("old client heartbeat incompatible")
	}
	resume := broker.ResumeRequest{Holder: g4FixtureHolder(), RegistrationID: registered.Session.RegistrationID, Harness: register.Harness, SessionID: register.SessionID, PID: register.PID, CWD: register.CWD, ClientID: register.ClientID, ChannelID: "activity-next", Secret: register.Secret, RequestID: "activity-resume", ExpectedRevision: registered.Session.Revision}
	if resumed := callInto[broker.RegisterResult](t, client, "resume_session", resume); resumed.ActivityVersion != 1 || resumed.Session.Activity.State != "unknown" {
		t.Fatal("resume did not advertise gate and reset evidence")
	}
	// An absent version in an old server result remains zero for future clients.
	var oldServer broker.RegisterResult
	if err := json.Unmarshal([]byte(`{"session":{"id":"old","name":"old","harness":"claude","status":"alive"}}`), &oldServer); err != nil || oldServer.ActivityVersion != 0 {
		t.Fatal("old server response incompatible")
	}
}
