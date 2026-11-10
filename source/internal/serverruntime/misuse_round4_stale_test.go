package serverruntime

import (
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMisuseK1AdminAndProjectReadsDoNotConfirmAnotherSender(t *testing.T) {
	store := protectedStore(t)
	remote := broker.NewRemoteService()
	defer remote.Close()
	b := newProtectedBackend("project", remote)
	sender, ss := manageRegistered(t, store, b, 311, "admin", "sender")
	old, os := manageRegistered(t, store, b, 312, "admin", "peer")
	ctx := access.WithGrant(t.Context(), sender)
	if out, err := b.SendMessage(ctx, broker.Message{From: ss.Name, To: os.Name, Body: "observe"}); err != nil || !out.Accepted {
		t.Fatal("observe")
	}
	if _, err := b.RenameSession(access.WithGrant(t.Context(), old), broker.RenameRequest{RegistrationID: os.RegistrationID, ChannelID: protectedRequest(312, "peer").ChannelID, Name: "old-peer"}); err != nil {
		t.Fatal(err)
	}
	_, replacement := manageRegistered(t, store, b, 313, "admin", "peer")
	for _, grant := range []access.Grant{
		{OwnerID: "admin", Scopes: []string{access.SessionRead, access.SessionManage}},
		{Project: "project", Scopes: []string{access.SessionRead}, Check: func(string) error { return errors.New("not session bound") }},
	} {
		if _, err := b.GetSession(access.WithGrant(t.Context(), grant), "peer"); err != nil {
			t.Fatal("unbound read", err)
		}
		out, err := b.SendMessage(ctx, broker.Message{From: ss.Name, To: replacement.Name, Body: "must remain fenced"})
		if err != nil || out.Accepted || out.Code != "recipient-changed" {
			t.Fatal("unbound authority cleared another registration's fence")
		}
	}
	other := newProtectedBackend("other-project", broker.NewRemoteService())
	defer other.Close()
	if got, err := other.GetSession(t.Context(), replacement.RegistrationID); err == nil || got.RegistrationID != "" {
		t.Fatal("cross-project registration disclosed")
	}
}

func TestMisuseK1IssuanceCredentialCannotConfirmPeer(t *testing.T) {
	r, projectToken := newDeviceRuntime(t)
	req := httptest.NewRequest("POST", "https://broker/mcp?project=test", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_session","arguments":{"name":"private-peer"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	r.bound(projectToken).ServeHTTP(w, req)
	if w.Code != 401 && w.Code != 403 {
		t.Fatal("issuance credential could access MCP", w.Code)
	}
	if strings.Contains(w.Body.String(), "private-peer") {
		t.Fatal("issuance denial reflected peer selector")
	}
}
