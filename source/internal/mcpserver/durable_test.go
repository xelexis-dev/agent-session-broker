package mcpserver

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

func durableClient(t *testing.T, backend Backend) *mcp.ClientSession {
	t.Helper()
	server := New(backend, &stubMemory{})
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.17.3"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func callInto[T any](t *testing.T, client *mcp.ClientSession, name string, args any) T {
	t.Helper()
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if result.IsError {
		t.Fatalf("%s tool error: %#v", name, result.Content)
	}
	var data []byte
	if result.StructuredContent != nil {
		data, err = json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
	} else if len(result.Content) > 0 {
		data = []byte(result.Content[0].(*mcp.TextContent).Text)
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("%s result %s: %v", name, data, err)
	}
	return out
}

func TestRemoteServerExposesDurableToolsWithCodeResults(t *testing.T) {
	ctx := context.Background()
	service := broker.NewRemoteService()
	client := durableClient(t, service)
	listed, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := []string{"attach_session", "get_bug_report", "get_session", "list_bug_reports", "list_sessions", "memory_get", "memory_put", "memory_search", "probe_response", "register_session", "rename_session", "renew_lease", "report_bug", "report_ingress_failure", "resume_session", "retire_session", "send_message", "set_bug_report_status", "stop_session", "suspend_session", "unsuspend_session", "wait_for_message"}
	if join(names) != join(want) {
		t.Fatalf("tools=%v want=%v", names, want)
	}
	for _, name := range []string{"register_session", "resume_session", "renew_lease", "probe_response"} {
		for _, tool := range listed.Tools {
			if tool.Name == name && !strings.Contains(tool.Description, "Not a model messaging tool") {
				t.Fatalf("%s description does not mark the tool internal: %q", name, tool.Description)
			}
		}
	}
	register := broker.RegisterRequest{Holder: g4FixtureHolder(), Harness: "claude", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: `C:\work`, ClientID: "client-alpha-0001", Name: "pc-a", ChannelID: "lease-1", Secret: strings.Repeat("s", 40), RequestID: "req-1"}
	registered := callInto[broker.RegisterResult](t, client, "register_session", register)
	if registered.Code != "" || registered.Session.RegistrationID == "" || !registered.Session.AutomaticDeliveryReady {
		t.Fatalf("register: %+v", registered)
	}
	raw, _ := json.Marshal(registered)
	if strings.Contains(string(raw), register.Secret) {
		t.Fatalf("secret echoed: %s", raw)
	}
	wrong := register
	wrong.Secret = strings.Repeat("x", 40)
	wrong.RequestID, wrong.ChannelID = "req-2", "lease-2"
	if r := callInto[broker.RegisterResult](t, client, "register_session", wrong); r.Code != broker.CodeOwned {
		t.Fatalf("wrong secret not reported as owned code: %+v", r)
	}
	callInto[broker.SendResult](t, client, "send_message", map[string]any{"from": "pc-a", "to": "pc-a", "body": "hello"})
	got := callInto[broker.Message](t, client, "wait_for_message", map[string]any{"recipient": "pc-a", "channelId": "lease-1"})
	if got.Body != "hello" {
		t.Fatal(got)
	}
	renewed := callInto[broker.RegisterResult](t, client, "renew_lease", broker.RenewRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: "lease-1"})
	if renewed.Code != "" || renewed.PendingProbe != nil {
		t.Fatalf("renew: %+v", renewed)
	}
	resumed := callInto[broker.RegisterResult](t, client, "resume_session", broker.ResumeRequest{Holder: g4FixtureHolder(), RegistrationID: registered.Session.RegistrationID, Harness: "claude", SessionID: register.SessionID, PID: 123, CWD: register.CWD, ClientID: register.ClientID, ChannelID: "lease-2", Secret: register.Secret, RequestID: "req-3", ExpectedRevision: 1})
	if resumed.Code != "" || resumed.Session.Revision != 2 {
		t.Fatalf("resume: %+v", resumed)
	}
	stale := callInto[broker.ProbeResult](t, client, "probe_response", broker.ProbeAnswer{RegistrationID: registered.Session.RegistrationID, ProbeID: "none", Revision: 2, ChannelID: "lease-2", OK: true})
	if stale.Code != broker.CodeStaleProbe {
		t.Fatalf("stale probe: %+v", stale)
	}
	failure := callInto[okOutput](t, client, "report_ingress_failure", map[string]any{"registrationId": registered.Session.RegistrationID, "channelId": "lease-2"})
	if !failure.OK {
		t.Fatalf("failure report by registration id rejected: %+v", failure)
	}
	sessions := callInto[[]broker.Session](t, client, "list_sessions", map[string]any{})
	if len(sessions) != 1 || sessions[0].ErrorCode != "ingress-failed" || sessions[0].Status != broker.StatusDegraded {
		t.Fatalf("list after failure: %+v", sessions)
	}
	callInto[okOutput](t, client, "stop_session", map[string]any{"name": "pc-a"})
	detached := callInto[broker.RegisterResult](t, client, "resume_session", broker.ResumeRequest{Holder: g4FixtureHolder(), RegistrationID: registered.Session.RegistrationID, Harness: "claude", SessionID: register.SessionID, PID: 123, CWD: register.CWD, ClientID: register.ClientID, ChannelID: "lease-3", Secret: register.Secret, RequestID: "req-4"})
	if detached.Code != broker.CodeDetached {
		t.Fatalf("detached resume: %+v", detached)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if callInto[broker.Session](t, client, "get_session", map[string]any{"name": "pc-a"}).ErrorCode == "detached" {
			return
		}
	}
	t.Fatal("stop_session did not mark the durable registration detached")
}
