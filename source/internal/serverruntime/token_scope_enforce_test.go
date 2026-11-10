package serverruntime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/memory"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

// Restricted snapshots are test fixtures only; issuance still produces LEGACY_V1.
func restrictRuntimeChild(t *testing.T, r *deviceRuntime, nickname string, scopes []string) {
	t.Helper()
	if r.close() != nil {
		t.Fatal("fixture shutdown failed")
	}
	db, err := sqlitestore.Open(filepath.Join(r.dir, "control"), "control.db")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := db.Get("state")
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if json.Unmarshal(raw, &st) != nil {
		t.Fatal("fixture decode failed")
	}
	found := false
	for _, p := range st["projects"].(map[string]any) {
		for _, item := range p.(map[string]any)["tokens"].([]any) {
			token := item.(map[string]any)
			if token["nickname"] == nickname && token["kind"] == "session" {
				token["scopes"] = scopes
				found = true
			}
		}
	}
	if !found {
		t.Fatal("child fixture missing")
	}
	raw, _ = json.Marshal(st)
	if db.Put("state", raw) != nil || db.Close() != nil {
		t.Fatal("fixture write failed")
	}
	r.start()
}

func TestScopeReadOnlyChildDeniesMutationsWithoutSideEffects(t *testing.T) {
	r, parent := newDeviceRuntime(t)
	req := protectedRequest(71, "ignored")
	child := r.authorize(parent, req, "reader")
	otherReq := protectedRequest(72, "ignored")
	other := r.authorize(parent, otherReq, "other")
	restrictRuntimeChild(t, r, "reader", []string{access.SessionRead, access.SessionRegister})
	registered := managedTool[broker.RegisterResult](t, r.bound(child), "register_session", req)
	managedTool[broker.RegisterResult](t, r.bound(other), "register_session", otherReq)
	for _, c := range []struct {
		name string
		args any
	}{
		{"memory_put", memory.PutInput{Title: "forbidden", Body: "scope-side-effect", CreatedBy: "reader"}},
		{"send_message", broker.Message{From: "reader", To: "other", Body: "forbidden"}},
		{"stop_session", map[string]string{"name": "reader"}},
	} {
		var result struct {
			IsError           bool
			StructuredContent json.RawMessage
			Content           []struct{ Text string }
		}
		managedRPC(t, r.bound(child), "tools/call", map[string]any{"name": c.name, "arguments": c.args}, &result)
		if !result.IsError || len(result.Content) != 1 || result.Content[0].Text != "insufficient_scope" || len(result.StructuredContent) > 0 {
			t.Errorf("%s: restricted child must receive only fixed insufficient_scope", c.name)
		}
	}
	reader := managedTool[broker.Session](t, r.bound(child), "get_session", map[string]string{"name": "reader"})
	target := managedTool[broker.Session](t, r.bound(other), "get_session", map[string]string{"name": "other"})
	if reader.Revision != registered.Session.Revision || reader.ErrorCode == broker.CodeDetached || target.PendingMessages != 0 {
		t.Error("denied stop/send changed registration or queue")
	}
	results := managedTool[memory.SearchResult](t, r.bound(other), "memory_search", memory.SearchInput{Query: "scope-side-effect"})
	if len(results.Nodes) != 0 {
		t.Error("denied memory_put created a node")
	}
}

func TestProtectedBackendScopeBoundaries(t *testing.T) {
	store := protectedStore(t)
	grant, _ := protectedGrant(t, store, "caller")
	raw := broker.NewRemoteService()
	defer raw.Close()
	b := newProtectedBackend("project", raw)
	req := protectedRequest(81, "ignored")
	registered, err := b.RegisterSession(access.WithGrant(t.Context(), grant), req)
	if err != nil || registered.Code != "" {
		t.Fatal("fixture register failed")
	}
	before, _ := raw.ListSessions(t.Context())
	// The backend must not rely on the MCP dispatch guard, and auto never
	// implies an ordinary permission. Empty/nil snapshots also deny.
	for _, scopes := range [][]string{nil, {access.SessionRegisterAuto}} {
		restricted := grant
		restricted.Scopes = scopes
		ctx := access.WithGrant(t.Context(), restricted)
		checks := map[string]func() error{
			"list":     func() error { _, e := b.ListSessions(ctx); return e },
			"get":      func() error { _, e := b.GetSession(ctx, "caller"); return e },
			"attach":   func() error { _, e := b.AttachSession(ctx, broker.AttachRequest{}); return e },
			"register": func() error { _, e := b.RegisterSession(ctx, req); return e },
			"resume":   func() error { _, e := b.ResumeSession(ctx, broker.ResumeRequest{}); return e },
			"renew":    func() error { _, e := b.RenewLease(ctx, broker.RenewRequest{}); return e },
			"probe":    func() error { _, e := b.AnswerProbe(ctx, broker.ProbeAnswer{}); return e },
			"failure":  func() error { return b.ReportIngressFailure(ctx, registered.Session.RegistrationID, req.ChannelID) },
			"rename":   func() error { _, e := b.RenameSession(ctx, broker.RenameRequest{}); return e },
			"stop":     func() error { return b.StopSession(ctx, "caller") },
			"send": func() error {
				_, e := b.SendMessage(ctx, broker.Message{From: "caller", To: "caller", Body: "denied"})
				return e
			},
			"sender": func() error { _, e := b.MessageSender(ctx, "caller"); return e },
			"wait":   func() error { _, e := b.WaitForMessage(ctx, "caller"); return e },
			"ack-ingress": func() error {
				_, e := b.WaitForIngressAck(ctx, protocol.IngressRequest{Recipient: "caller", ChannelID: req.ChannelID})
				return e
			},
			"ingress": func() error { _, e := b.WaitForIngress(ctx, "caller", req.ChannelID); return e },
		}
		for name, call := range checks {
			if !errors.Is(call(), access.ErrInsufficientScope) {
				t.Errorf("%s backend did not enforce scope", name)
			}
		}
	}
	after, _ := raw.ListSessions(t.Context())
	if !reflect.DeepEqual(before, after) {
		t.Fatal("denied backend call changed state")
	}
	// Valid scope is not permission to use someone else's object.
	other := protectedRequest(82, "other")
	protectedRegistered(t, raw, other)
	grant.Scopes = []string{access.MessageSend}
	ctx := access.WithGrant(context.Background(), grant)
	if _, e := b.MessageSender(ctx, "other"); !errors.Is(e, errProtectedAccess) {
		t.Fatal("send-only lookup bypassed binding")
	}
}

func TestScopeSendFromPendingSenderWithoutReceive(t *testing.T) {
	r, parent := newDeviceRuntime(t)
	req := protectedRequest(91, "ignored")
	req.IngressPending = true
	child := r.authorize(parent, req, "sender")
	otherReq := protectedRequest(92, "ignored")
	other := r.authorize(parent, otherReq, "recipient")
	restrictRuntimeChild(t, r, "sender", []string{access.MessageSend, access.SessionRegister})
	registered := managedTool[broker.RegisterResult](t, r.bound(child), "register_session", req)
	if registered.Code != "" || registered.Session.AutomaticDeliveryReady {
		t.Fatal("register without read/receive must stay ingress-pending")
	}
	managedTool[broker.RegisterResult](t, r.bound(other), "register_session", otherReq)
	sent := managedTool[broker.SendResult](t, r.bound(child), "send_message", broker.Message{From: "sender", To: "recipient", Body: "send-only"})
	if !sent.Accepted {
		t.Fatal("send incorrectly required roster read")
	}
	got := managedTool[broker.Message](t, r.bound(other), "wait_for_message", map[string]string{"recipient": "recipient", "channelId": otherReq.ChannelID})
	if got.Body != "send-only" {
		t.Fatal("message lost")
	}
	renewed := managedTool[broker.RegisterResult](t, r.bound(child), "renew_lease", broker.RenewRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: req.ChannelID, IngressPending: true})
	if renewed.Code != "" || renewed.Session.AutomaticDeliveryReady {
		t.Fatal("renew must preserve pending sender without read/receive")
	}
	for _, tc := range []struct {
		name string
		args any
	}{
		{"list_sessions", struct{}{}},
		{"get_session", map[string]string{"name": "sender"}},
		{"wait_for_message", map[string]string{"recipient": "sender", "channelId": req.ChannelID}},
	} {
		var result struct {
			IsError bool
			Content []struct{ Text string }
		}
		managedRPC(t, r.bound(child), "tools/call", map[string]any{"name": tc.name, "arguments": tc.args}, &result)
		if !result.IsError || len(result.Content) != 1 || result.Content[0].Text != "insufficient_scope" {
			t.Errorf("%s scope bypass", tc.name)
		}
	}
}
