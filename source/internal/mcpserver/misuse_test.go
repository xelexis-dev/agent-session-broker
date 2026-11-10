package mcpserver

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/memory"
)

func misuseRemote(t *testing.T) (*broker.RemoteService, *mcp.ClientSession, broker.RegisterRequest, broker.Session) {
	t.Helper()
	remote := broker.NewRemoteService()
	t.Cleanup(func() { _ = remote.Close() })
	req := broker.RegisterRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: "/synthetic", ClientID: "misuse-client", Name: "alpha", ChannelID: "misuse-lease", Secret: strings.Repeat("s", 40), RequestID: "misuse-register"}
	registered, err := remote.RegisterSession(t.Context(), req)
	if err != nil || registered.Code != "" {
		t.Fatal("fixture registration failed")
	}
	return remote, durableClient(t, remote), req, registered.Session
}

func misuseRejected(t *testing.T, client *mcp.ClientSession, name string, args any) {
	t.Helper()
	result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err == nil && !result.IsError {
		t.Fatalf("%s accepted invalid input", name)
	}
}

func TestMisuseA1RenameBoundariesPreserveRegistration(t *testing.T) {
	remote, client, req, before := misuseRemote(t)
	other := req
	other.SessionID = "22222222-2222-4222-8222-222222222222"
	other.ClientID, other.Name, other.ChannelID, other.RequestID = "other-client", "beta", "other-lease", "other-request"
	if result, err := remote.RegisterSession(t.Context(), other); err != nil || result.Code != "" {
		t.Fatal("other fixture failed")
	}
	for _, name := range []string{"", " ", strings.Repeat("x", 129), "line\nbreak", "nul\x00name", "ctrl\x1bname", "beta", "broker-admin", "BROKER-ADMIN", "registration:alpha"} {
		misuseRejected(t, client, "rename_session", broker.RenameRequest{RegistrationID: before.RegistrationID, ChannelID: req.ChannelID, Name: name})
		after, err := remote.GetSession(t.Context(), before.RegistrationID)
		if err != nil || after.Name != before.Name || after.Revision != before.Revision {
			t.Fatal("rejected rename changed registration")
		}
	}
	// Names are case-sensitive labels. Generated-looking prefixes are not
	// reserved namespaces or proof of identity.
	for _, name := range []string{"alpha", "Alpha", "Beta", "auto-worker", "message-sender-worker", strings.Repeat("x", 128)} {
		got := callInto[broker.Session](t, client, "rename_session", broker.RenameRequest{RegistrationID: before.RegistrationID, ChannelID: req.ChannelID, Name: name})
		if got.RegistrationID != before.RegistrationID || got.Generation != before.Generation || got.Name != name {
			t.Fatal("rename changed identity or lease")
		}
	}
	for i := 0; i < 20; i++ {
		callInto[broker.Session](t, client, "rename_session", broker.RenameRequest{RegistrationID: before.RegistrationID, ChannelID: req.ChannelID, Name: fmt.Sprintf("rapid-%d", i)})
	}
	if _, err := remote.SendMessage(t.Context(), broker.Message{From: "rapid-19", To: "rapid-19", Body: "after rename"}); err != nil {
		t.Fatal("valid delivery failed after rename burst")
	}
}

func TestMisuseA2OpaqueBodiesAndQueue(t *testing.T) {
	remote, client, req, session := misuseRemote(t)
	send := func(body string) (broker.SendResult, error) {
		return remote.SendMessage(t.Context(), broker.Message{From: session.Name, To: session.Name, Body: body})
	}
	misuseRejected(t, client, "send_message", map[string]any{"from": session.Name, "to": "absent", "body": "body"})
	if _, err := send(""); err == nil {
		t.Fatal("empty body accepted")
	}
	// Opaque message bodies are deliberately preserved, including whitespace,
	// JSON-escaped controls and user-provided token-shaped text.
	for _, text := range []string{"   ", "nul\x00control\x1b", strings.Join([]string{"asb", "p", "syntheticbody"}, "_")} {
		if _, err := send(text); err != nil {
			t.Fatal("opaque body rejected")
		}
		got, err := remote.WaitForIngress(t.Context(), session.Name, req.ChannelID)
		if err != nil || got.Body != text {
			t.Fatal("opaque body changed")
		}
	}
	for i := 0; i < 32; i++ {
		if _, err := send("queued"); err != nil {
			t.Fatal("queue filled early")
		}
	}
	if _, err := send("overflow"); err != broker.ErrQueueFull {
		t.Fatal("33rd message did not report queue full")
	}
	current, _ := remote.GetSession(t.Context(), session.RegistrationID)
	if current.PendingMessages != 32 {
		t.Fatal("overflow changed queue")
	}
}

func TestMisuseA3MemoryInputsAndLiteralQueries(t *testing.T) {
	store, err := memory.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	client := catalogSession(t, New(&stubBackend{}, store))
	for _, id := range []string{"", "../outside", "/", "\x00", strings.Repeat("x", 10000)} {
		misuseRejected(t, client, "memory_get", map[string]any{"id": id})
	}
	for _, in := range []memory.PutInput{{}, {Title: "valid", Body: strings.Repeat("x", memory.MaxBodyBytes+1), CreatedBy: "worker"}, {Title: strings.Repeat("x", memory.MaxTitleRunes+1), Body: "body", CreatedBy: "worker"}, {Title: "valid", Body: "body", CreatedBy: "worker", Links: []string{"../outside"}}} {
		misuseRejected(t, client, "memory_put", in)
	}
	for i := 0; i < 40; i++ {
		callInto[memory.Node](t, client, "memory_put", memory.PutInput{Title: fmt.Sprintf("node-%d", i), Body: "literal % _ \" * OR NEAR() 가", CreatedBy: "worker"})
	}
	for _, q := range []string{"", "%", "_", "\"", "*", "OR", "NEAR()", "unterminated\"", strings.Repeat("가", 10000)} {
		result := callInto[memory.SearchResult](t, client, "memory_search", memory.SearchInput{Query: q, Limit: 100})
		if len(result.Warnings) != 0 {
			t.Fatal("literal query produced search warning")
		}
		want := 40
		if q == "unterminated\"" || len(q) > 10000 {
			want = 0
		}
		if len(result.Nodes) != want {
			t.Fatalf("literal query result count=%d want=%d", len(result.Nodes), want)
		}
	}
}

func TestMisuseA8MalformedCallsLeaveServerUsable(t *testing.T) {
	_, client, _, session := misuseRemote(t)
	for _, tc := range []struct {
		name string
		args any
	}{
		{"send_message", map[string]any{"from": 7, "to": session.Name, "body": "x"}},
		{"send_message", map[string]any{"from": session.Name, "to": session.Name}},
		{"send_message", map[string]any{"from": session.Name, "to": session.Name, "body": "x", "unknown": true}},
		{"send_message", json.RawMessage(`null`)},
		{"send_message", map[string]any{"from": session.Name, "to": session.Name, "body": strings.Repeat("x", 1024*1024)}},
		{"unknown_tool", map[string]any{}},
	} {
		misuseRejected(t, client, tc.name, tc.args)
	}
	got := callInto[broker.Session](t, client, "get_session", nameInput{Name: session.Name})
	if got.RegistrationID != session.RegistrationID || got.PendingMessages != 0 {
		t.Fatal("malformed calls corrupted server state")
	}
}

func TestMisuseA1RejectsInvisibleNicknameWithoutChangingState(t *testing.T) {
	remote, client, req, before := misuseRemote(t)
	for _, name := range []string{"zero\u200bwidth", "rtl\u202ename", "isolate\u2066name", "bom\ufeffname"} {
		result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "rename_session", Arguments: broker.RenameRequest{RegistrationID: before.RegistrationID, ChannelID: req.ChannelID, Name: name}})
		if err == nil && !result.IsError {
			t.Error("rename accepted invisible Unicode format character")
		}
		after, _ := remote.GetSession(t.Context(), before.RegistrationID)
		if after.Name != before.Name || after.Revision != before.Revision {
			t.Error("invalid rename changed state")
		}
	}
}
