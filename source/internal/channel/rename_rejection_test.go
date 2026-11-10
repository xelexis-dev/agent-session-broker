package channel

import (
	"context"
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type renameReplyUpstream struct{ result *mcp.CallToolResult }

func (u renameReplyUpstream) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return u.result, nil
}

func TestRenameRejectionGuidesOrphanRetirement(t *testing.T) {
	u := renameReplyUpstream{&mcp.CallToolResult{IsError: true, StructuredContent: map[string]any{"code": "nickname-in-use", "holder": "orphaned-credential", "message": "synthetic-private-server-message"}}}
	original := protocol.Session{Name: "unchanged"}
	err := call(t.Context(), u, "rename_session", protocol.RenameRequest{Name: "held"}, &original)
	if err == nil || !strings.Contains(err.Error(), "retire_session") || !strings.Contains(err.Error(), "name") {
		t.Fatal("missing orphan retirement guidance", err)
	}
	if strings.Contains(err.Error(), "synthetic-private-server-message") {
		t.Fatal("server message exposed")
	}
	if original.Name != "unchanged" {
		t.Fatal("rejection overwrote local session")
	}
}

func TestRenameRejectionHolderAllowlist(t *testing.T) {
	const id = "AAAAAAAAAAAAAAAAAAAAAAAAAA"
	for _, tc := range []struct {
		holder, fragment string
		registration     bool
	}{
		{"live-session", "Choose another nickname", true},
		{"inactive-registration", "an inactive registration", true},
		{"detached-registration", "a detached registration", true},
		{"orphaned-credential", "name set to the requested nickname", false},
		{"unused-credential", "administration panel", false},
	} {
		t.Run(tc.holder, func(t *testing.T) {
			reply := &mcp.CallToolResult{IsError: true, StructuredContent: map[string]any{"code": "nickname-in-use", "holder": tc.holder, "holderRegistrationId": id, "message": "synthetic-hidden-message"}}
			err := call(t.Context(), renameReplyUpstream{reply}, "rename_session", nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.fragment) {
				t.Fatal("wrong holder guidance", err)
			}
			if strings.Contains(err.Error(), id) != tc.registration {
				t.Fatal("holder ID used for wrong mode", err)
			}
			if strings.Contains(err.Error(), "synthetic-hidden-message") {
				t.Fatal("server text exposed")
			}
		})
	}
}

func TestRenameRejectionOldMalformedAndSecretResponses(t *testing.T) {
	const generic = "broker rename_session rejected the request; server gave no reason"
	for _, tc := range []struct {
		name       string
		structured any
		text       string
	}{
		{"old-server", nil, "nickname already in use: synthetic-hidden-message"},
		{"no-result", nil, ""},
		{"unknown-code", map[string]any{"code": "synthetic-hidden-message", "holder": "live-session"}, ""},
		{"unknown-holder", map[string]any{"code": "nickname-in-use", "holder": "synthetic-hidden-message"}, ""},
		{"wrong-type", map[string]any{"code": "nickname-in-use", "holder": 42}, ""},
		{"malformed-text", nil, `{"code":"nickname-in-use"`},
		{"oversized-text", nil, strings.Repeat("x", 4097)},
		{"oversized-structured", map[string]any{"code": "nickname-in-use", "holder": "live-session", "message": strings.Repeat("x", 4097)}, ""},
		{"invalid-structured-before-text", []string{"not-an-object"}, `{"code":"nickname-in-use","holder":"live-session"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply := &mcp.CallToolResult{IsError: true, StructuredContent: tc.structured, Content: []mcp.Content{&mcp.TextContent{Text: tc.text}}}
			err := call(t.Context(), renameReplyUpstream{reply}, "rename_session", nil, nil)
			if err == nil || err.Error() != generic {
				t.Fatal("unsafe generic guidance", err)
			}
		})
	}
}

func TestRenameRejectionTextFallbackAndInvalidID(t *testing.T) {
	for _, structured := range []bool{false, true} {
		reason := map[string]any{"code": "nickname-in-use", "holder": "detached-registration", "holderRegistrationId": "synthetic-hidden-message", "message": "synthetic-hidden-message"}
		reply := &mcp.CallToolResult{IsError: true}
		if structured {
			reply.StructuredContent = reason
		} else {
			data, _ := json.Marshal(reason)
			reply.Content = []mcp.Content{&mcp.TextContent{Text: string(data)}}
		}
		err := call(t.Context(), renameReplyUpstream{reply}, "rename_session", nil, nil)
		if err == nil || !strings.Contains(err.Error(), "a detached registration") || !strings.Contains(err.Error(), "list_sessions") {
			t.Fatal("safe fallback lost holder guidance", err)
		}
		if strings.Contains(err.Error(), "synthetic-hidden-message") {
			t.Fatal("invalid ID or server text exposed")
		}
	}
}

func TestRenameModelGuidancePreservesSessionOnRejection(t *testing.T) {
	dir := t.TempDir()
	f := newRenameFixture(t, "codex", dir, newDeviceAuthServer(t), newSwitchableUpstream(t))
	request := protocol.RegisterRequest{Harness: "codex", SessionID: "22222222-2222-4222-8222-222222222222", PID: 77, CWD: "/synthetic/other", ClientID: "client-other-000001", Name: "held", ChannelID: "other-lease", Secret: strings.Repeat("o", 40), RequestID: "other-request"}
	var other protocol.RegisterResult
	if err := call(t.Context(), f.upstream, "register_session", request, &other); err != nil || other.Code != "" {
		t.Fatal("holder registration failed", err)
	}
	result := f.call("", "rename_session", map[string]string{"name": "held"})
	if !result.IsError {
		t.Fatal("conflicting rename succeeded")
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "live session") || !strings.Contains(text, "retire_session") || !strings.Contains(text, other.Session.RegistrationID) {
		t.Fatal("model lacks actionable reason", text)
	}
	if f.status("")["name"] != "before" {
		t.Fatal("rejection changed binding name")
	}
	f.send("before", "kept")
	select {
	case m := <-f.sinks[f.thread]:
		if m.Body != "kept" {
			t.Fatal("unexpected envelope")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("rejection stopped delivery")
	}
	f.stop()
	state, err := clientstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	clientID, err := state.ClientID()
	if err != nil {
		t.Fatal(err)
	}
	saved, ok, err := state.LoadSession(clientstate.SessionKey("https://broker/mcp?project=brk", "codex", f.thread, clientID))
	if err != nil || !ok || saved.Registration.Name != "before" || saved.Registration.RegistrationID != f.original.RegistrationID || saved.Credential.Nickname != "before" {
		t.Fatal("rejection changed saved state")
	}
}

func TestRegistrationNotFoundMentionsRetirement(t *testing.T) {
	text := autoRecoveryGuidance("registration-not-found")
	for _, part := range []string{"another session may have retired it", "No new registration was created", "attach_session"} {
		if !strings.Contains(text, part) {
			t.Fatal("missing recovery guidance", part)
		}
	}
}
