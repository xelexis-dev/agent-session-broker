package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConversationHandoverSameClaudeProcessKeepsRegistration(t *testing.T) {
	t.Parallel()
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	oldID := broker.AttachRequest{Harness: "claude", SessionID: "11111111-1111-4111-8111-111111111111", PID: os.Getpid(), CWD: t.TempDir()}
	options := func(o *durableOptions) {
		o.parentProcessKey = func(context.Context, int) (string, error) { return "synthetic-process-key", nil }
	}
	first := r.identityHarness(t, dir, oldID, options)
	first.attach(t)
	original := first.session
	first.close()
	nextID := oldID
	nextID.SessionID = "22222222-2222-4222-8222-222222222222"
	next := r.identityHarness(t, dir, nextID, options)
	before := r.direct.Load()
	if next.status(t).Registered || r.direct.Load() != before {
		t.Fatal("new conversation registered before explicit attach")
	}
	attachHandover(t, next, map[string]any{})
	if next.session.RegistrationID != original.RegistrationID || next.session.Name != original.Name {
		t.Fatalf("conversation switch created a new registration or name")
	}
	store, err := clientstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cid, err := store.ClientID()
	if err != nil {
		t.Fatal(err)
	}
	oldKey := clientstate.SessionKey(brokerScope(r.endpoint, "detach"), "claude", oldID.SessionID, cid)
	saved, _, err := store.LoadSession(oldKey)
	if err != nil || saved.Registration.HandedOverTo == "" {
		t.Fatal("old conversation was not marked handed over")
	}
	if next.status(t).HandoverFrom == nil || next.session.HandoverFrom == nil {
		t.Fatal("handover source missing")
	}
}

func attachHandover(t *testing.T, h *detachedHarness, args map[string]any) {
	t.Helper()
	result, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "attach_session", Arguments: args})
	if err != nil {
		t.Fatal("local attach RPC failed")
	}
	if result.IsError {
		for _, c := range result.Content {
			if text, ok := c.(*mcp.TextContent); ok {
				t.Log("safe local attach error:", text.Text)
			}
		}
		t.Fatal("local attach refused")
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &h.session); err != nil {
		t.Fatal(err)
	}
}

func claudeHandoverIdentity(n, pid int) broker.AttachRequest {
	return broker.AttachRequest{Harness: "claude", SessionID: fmt.Sprintf("%08d-1111-4111-8111-111111111111", n), PID: pid, CWD: "/synthetic/handover"}
}
func handoverOptions(o *durableOptions) {
	o.parentProcessKey = func(context.Context, int) (string, error) { return "synthetic-process", nil }
}
func handoverSaved(t *testing.T, r *detachedRuntime, dir string, id broker.AttachRequest) (*clientstate.Store, string, clientstate.SessionState) {
	t.Helper()
	store, err := clientstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cid, err := store.ClientID()
	if err != nil {
		t.Fatal(err)
	}
	key := clientstate.SessionKey(brokerScope(r.endpoint, "detach"), "claude", id.SessionID, cid)
	saved, found, err := store.LoadSession(key)
	if err != nil || !found {
		t.Fatal("saved conversation unavailable")
	}
	return store, key, saved
}
func handoverRefusal(t *testing.T, h *detachedHarness, name, reason string) {
	t.Helper()
	out, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]string{"takeover": name}})
	if err != nil || !out.IsError {
		t.Fatal("explicit takeover did not fail closed")
	}
	text := ""
	for _, c := range out.Content {
		if v, ok := c.(*mcp.TextContent); ok {
			text += v.Text
		}
	}
	if !strings.Contains(text, "handover-refused: "+reason) {
		t.Fatalf("missing safe handover refusal: expected %s", reason)
	}
}

type handoverBlockedReceiver struct{ *brokerUpstream }

func (u *handoverBlockedReceiver) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name == "wait_for_message" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return u.brokerUpstream.CallTool(ctx, p)
}
func TestConversationHandoverKeepsQueuedMessagesAndRevokesOldCredential(t *testing.T) {
	r := newDetachedRuntime(t)
	manager := r.harness(t, t.TempDir(), 991)
	manager.attach(t)
	dir := t.TempDir()
	oldID := claudeHandoverIdentity(1, 1234)
	old := r.identityHarness(t, dir, oldID, func(o *durableOptions) {
		handoverOptions(o)
		original := o.device.newUpstream
		o.device.newUpstream = func(token string) Upstream { return &handoverBlockedReceiver{original(token).(*brokerUpstream)} }
	})
	old.attach(t)
	_, _, saved := handoverSaved(t, r, dir, oldID)
	if err := call(t.Context(), manager.client, "send_message", broker.Message{From: manager.session.Name, To: old.session.Name, Body: "queued-before-switch"}, nil); err != nil {
		t.Fatal("queue setup failed")
	}
	var queued broker.Session
	if err := call(t.Context(), manager.client, "get_session", map[string]string{"name": old.session.RegistrationID}, &queued); err != nil || queued.PendingMessages != 1 {
		t.Fatal("message did not remain queued")
	}
	nextID := claudeHandoverIdentity(2, 1234)
	next := r.identityHarness(t, dir, nextID, handoverOptions)
	next.attach(t)
	if next.session.RegistrationID != old.session.RegistrationID || next.session.Name != old.session.Name {
		t.Fatal("queue handover changed registration")
	}
	select {
	case m := <-next.received:
		if m.Body != "queued-before-switch" {
			t.Fatal("wrong queued message")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued message not handed over")
	}
	probe := newTestBrokerUpstream(t.Context(), r.endpoint)
	defer probe.Close()
	probe.setBearerToken(saved.Credential.AccessToken)
	_, err := probe.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
	if err == nil || !probe.credentialRejected() {
		t.Fatal("old credential remained accepted")
	}
}
func TestConversationHandoverExplicitMissingAndUnsupportedFailClosed(t *testing.T) {
	t.Parallel()
	r := newDetachedRuntime(t)
	for _, tc := range []struct {
		name      string
		configure func(*durableOptions)
		reason    string
	}{{"missing", nil, "not-found"}, {"configured", func(o *durableOptions) { o.device = nil }, "unsupported-mode"}} {
		t.Run(tc.name, func(t *testing.T) {
			h := r.identityHarness(t, t.TempDir(), claudeHandoverIdentity(10, 1111), func(o *durableOptions) {
				handoverOptions(o)
				if tc.configure != nil {
					tc.configure(o)
				}
			})
			before := r.direct.Load()
			handoverRefusal(t, h, "not-in-local-state", tc.reason)
			_ = h.status(t)
			if r.direct.Load() != before {
				t.Fatal("failed explicit takeover started admission")
			}
		})
	}
}
func TestConversationHandoverOtherPIDAliveExplicitRefusesAutoFallsBack(t *testing.T) {
	t.Parallel()
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	oldID := claudeHandoverIdentity(20, 1001)
	old := r.identityHarness(t, dir, oldID, handoverOptions)
	old.attach(t)
	store, key, state := handoverSaved(t, r, dir, oldID)
	// A stale/copied local candidate cannot override the server's actual PID.
	state.Registration.ParentPID = 1002
	if err := store.SaveSession(key, state); err != nil {
		t.Fatal(err)
	}
	next := r.identityHarness(t, dir, claudeHandoverIdentity(21, 1002), handoverOptions)
	before := r.direct.Load()
	handoverRefusal(t, next, old.session.Name, "still-receiving")
	_ = next.status(t)
	if r.direct.Load() != before+1 {
		t.Fatal("explicit refusal allocated or retried admission")
	}
	next.attach(t)
	if next.session.RegistrationID == old.session.RegistrationID {
		t.Fatal("live other PID was taken over")
	}
	status := next.status(t)
	if status.Diagnostics == nil || status.Diagnostics.HandoverSkipped != "still-receiving" {
		t.Fatal("automatic fallback skip reason missing")
	}
}
func TestConversationHandoverDetachedExplicitAndAuditPrivacy(t *testing.T) {
	r := newDetachedRuntime(t)
	manager := r.harness(t, t.TempDir(), 992)
	manager.attach(t)
	dir := t.TempDir()
	oldID := claudeHandoverIdentity(30, 2001)
	old := r.identityHarness(t, dir, oldID, handoverOptions)
	old.attach(t)
	sourceStore, sourceKey, _ := handoverSaved(t, r, dir, oldID)
	if err := call(t.Context(), manager.client, "stop_session", map[string]string{"name": old.session.RegistrationID}, nil); err != nil {
		t.Fatal(err)
	}
	// Persisted detach must not make explicit takeover depend on receiver timing.
	waitFor(t, "old receiver saved detached observation", func() bool {
		saved, found, err := sourceStore.LoadSession(sourceKey)
		return err == nil && found && saved.Registration.Detached
	})
	old.close()
	nextID := claudeHandoverIdentity(31, 2002)
	next := r.identityHarness(t, dir, nextID, handoverOptions)
	attachHandover(t, next, map[string]any{"takeover": old.session.Name})
	if next.session.RegistrationID != old.session.RegistrationID {
		t.Fatal("detached takeover changed ID")
	}
	db, err := sqlitestore.Open(filepath.Join(r.root, "control"), "control.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.List()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for key, data := range rows {
		if !strings.HasPrefix(key, "audit/") {
			continue
		}
		if strings.Contains(string(data), oldID.SessionID) || strings.Contains(string(data), nextID.SessionID) {
			t.Fatal("audit exposed raw conversation identity")
		}
		var e controlpanel.AuditEvent
		if json.Unmarshal(data, &e) != nil {
			t.Fatal("audit decode")
		}
		if e.Kind == "handover" {
			found = true
			if e.Handover == nil || e.Handover.RegistrationID != old.session.RegistrationID || len(e.Handover.Previous) != 64 || len(e.Handover.Current) != 64 {
				t.Fatal("handover audit incomplete")
			}
		}
	}
	if !found {
		t.Fatal("handover audit missing")
	}
}
func TestConversationHandoverDetachedAutomaticFailsClosed(t *testing.T) {
	r := newDetachedRuntime(t)
	manager := r.harness(t, t.TempDir(), 993)
	manager.attach(t)
	dir := t.TempDir()
	oldID := claudeHandoverIdentity(32, 2003)
	old := r.identityHarness(t, dir, oldID, handoverOptions)
	old.attach(t)
	store, key, _ := handoverSaved(t, r, dir, oldID)
	if err := call(t.Context(), manager.client, "stop_session", map[string]string{"name": old.session.RegistrationID}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "old receiver saved detached observation", func() bool {
		saved, found, err := store.LoadSession(key)
		return err == nil && found && saved.Registration.Detached
	})
	old.close()
	next := r.identityHarness(t, dir, claudeHandoverIdentity(33, oldID.PID), handoverOptions)
	before := r.direct.Load()
	out, err := next.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
	if err != nil || !out.IsError {
		t.Fatal("automatic handover accepted detached record")
	}
	if r.direct.Load() != before || next.status(t).Registered {
		t.Fatal("detached refusal fell through to fresh admission")
	}
}

func TestConversationHandoverManualModeExplicitFailsAndAutoFallsBack(t *testing.T) {
	t.Parallel()
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	oldID := claudeHandoverIdentity(40, 3001)
	old := r.identityHarness(t, dir, oldID, handoverOptions)
	old.attach(t)
	old.close()
	next := r.identityHarness(t, dir, claudeHandoverIdentity(41, 3001), func(o *durableOptions) {
		handoverOptions(o)
		o.device.authorizer = newDeviceAuthClient(r.endpoint, r.manualToken, nil)
	})
	handoverRefusal(t, next, old.session.Name, "unsupported-mode")
	result, err := next.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
	if err != nil || !result.IsError {
		t.Fatal("manual approval should remain pending")
	}
	status := next.status(t)
	if status.Status != authPending || status.Diagnostics == nil || status.Diagnostics.HandoverSkipped != "unsupported-mode" {
		t.Fatal("manual fallback/diagnostic not preserved")
	}
}

func TestConversationHandoverLostResponseReusesCommittedRequest(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	oldID := claudeHandoverIdentity(50, 4001)
	old := r.identityHarness(t, dir, oldID, handoverOptions)
	old.attach(t)
	original := old.session
	old.close()
	next := r.identityHarness(t, dir, claudeHandoverIdentity(51, 4001), handoverOptions)
	r.dropHandover.Store(true)
	next.attach(t)
	if next.session.RegistrationID != original.RegistrationID || next.session.HandoverFrom == nil || next.session.HandoverFrom.Revision != original.Revision {
		t.Fatal("lost-response retry lost registration/source revision")
	}
}
func TestConversationHandoverAnotherClientLocalStateNotEligible(t *testing.T) {
	t.Parallel()
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	oldID := claudeHandoverIdentity(60, 5001)
	old := r.identityHarness(t, dir, oldID, handoverOptions)
	old.attach(t)
	old.close()
	store, key, state := handoverSaved(t, r, dir, oldID)
	state.ClientID = "different-client"
	if err := store.SaveSession(key, state); err != nil {
		t.Fatal(err)
	}
	next := r.identityHarness(t, dir, claudeHandoverIdentity(61, 5002), handoverOptions)
	before := r.direct.Load()
	handoverRefusal(t, next, old.session.Name, "not-found")
	if r.direct.Load() != before {
		t.Fatal("foreign local state sent handover request")
	}
}
func TestConversationHandoverOtherProjectCannotProveRegistration(t *testing.T) {
	t.Parallel()
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	oldID := claudeHandoverIdentity(70, 6001)
	old := r.identityHarness(t, dir, oldID, handoverOptions)
	old.attach(t)
	_, _, state := handoverSaved(t, r, dir, oldID)
	endpoint := strings.Replace(r.endpoint, "project=detach", "project=other", 1)
	client := newDeviceAuthClient(endpoint, r.otherToken, nil)
	_, err := client.exchange(t.Context(), directCodeRequest{Handover: true, PID: 6001, CWD: "/synthetic", RegistrationID: old.session.RegistrationID, Secret: state.Registration.Secret, deviceCodeRequest: deviceCodeRequest{Harness: "claude", SessionID: "88888888-1111-4111-8111-111111111111", ClientID: state.ClientID, RequestID: "cross-project"}})
	var refused broker.HandoverRefusal
	if !errors.As(err, &refused) || refused.Reason != "not-found" {
		t.Fatal("other project accepted ownership proof")
	}
}

func TestConversationHandoverSuspendedExplicitFailsClosed(t *testing.T) {
	t.Parallel()
	r := newDetachedRuntime(t)
	manager := r.harness(t, t.TempDir(), 993)
	manager.attach(t)
	dir := t.TempDir()
	oldID := claudeHandoverIdentity(80, 7001)
	old := r.identityHarness(t, dir, oldID, handoverOptions)
	old.attach(t)
	if err := call(t.Context(), manager.client, "suspend_session", map[string]string{"name": old.session.RegistrationID}, nil); err != nil {
		t.Fatal(err)
	}
	next := r.identityHarness(t, dir, claudeHandoverIdentity(81, 7002), handoverOptions)
	before := r.direct.Load()
	handoverRefusal(t, next, old.session.Name, "suspended")
	if next.status(t).Registered || r.direct.Load() != before+1 {
		t.Fatal("explicit suspended refusal started fresh registration")
	}
}
func TestConversationHandoverPIDReuseWithDifferentProcessKeyDoesNotAutoSelect(t *testing.T) {
	t.Parallel()
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	oldID := claudeHandoverIdentity(90, 8001)
	old := r.identityHarness(t, dir, oldID, handoverOptions)
	old.attach(t)
	old.close()
	next := r.identityHarness(t, dir, claudeHandoverIdentity(91, 8001), func(o *durableOptions) {
		o.parentProcessKey = func(context.Context, int) (string, error) { return "different-process-key", nil }
	})
	before := r.direct.Load()
	next.attach(t)
	if next.session.RegistrationID == old.session.RegistrationID || r.direct.Load() != before+1 {
		t.Fatal("PID reuse selected another process's conversation")
	}
}
