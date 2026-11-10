package serverruntime

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
)

func managedRPC(t *testing.T, h http.Handler, method string, params any, out any) {
	t.Helper()
	if method == "tools/call" {
		p := params.(map[string]any)
		meta, _ := p["_meta"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
			p["_meta"] = meta
		}
		meta["io.modelcontextprotocol/clientInfo"] = map[string]string{"name": "runtime-test", "version": "0.17.3"}
	}
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "https://broker/mcp?project=test", bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("%s: HTTP %d %s", method, w.Code, w.Body.String())
	}
	var envelope struct {
		Result json.RawMessage
		Error  json.RawMessage
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Error) > 0 {
		t.Fatalf("%s: %s", method, envelope.Error)
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Result, out); err != nil {
			t.Fatal(err)
		}
	}
}

func managedTool[T any](t *testing.T, h http.Handler, name string, arguments any) T {
	t.Helper()
	var result struct {
		IsError           bool
		StructuredContent json.RawMessage
		Content           []struct{ Text string }
	}
	managedRPC(t, h, "tools/call", map[string]any{"name": name, "arguments": arguments}, &result)
	if result.IsError {
		t.Fatalf("%s failed: %+v", name, result.Content)
	}
	data := result.StructuredContent
	if len(data) == 0 && len(result.Content) > 0 {
		data = []byte(result.Content[0].Text)
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("%s result: %v", name, err)
	}
	return out
}

func TestManagedDefaultRuntimeRestoresRegistrationMissingThenResumes(t *testing.T) {
	directory := t.TempDir()
	store, err := controlpanel.OpenSQLite(filepath.Join(directory, "control"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Setup("test-only-password-for-runtime"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateProject("test"); err != nil {
		t.Fatal(err)
	}
	token, err := store.IssueToken("test", "pc-one")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	start := func() (http.Handler, func() error) {
		h, close, err := newManagedHandler(directory, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { close() })
		bound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
			h.ServeHTTP(w, r)
		})
		managedRPC(t, bound, "initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "runtime-test", "version": "1"}}, nil)
		return bound, close
	}
	h, close := start()
	request := broker.RegisterRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: directory, ClientID: "runtime-client-001", Name: "pc-one", ChannelID: "lease-one", Secret: strings.Repeat("s", 40), RequestID: "request-one"}
	first := managedTool[broker.RegisterResult](t, h, "register_session", request)
	if first.Code != "" || first.Session.RegistrationID == "" {
		t.Fatalf("register: %+v", first)
	}
	if err := close(); err != nil {
		t.Fatal(err)
	}
	h, close = start()
	defer close()
	list := managedTool[[]broker.Session](t, h, "list_sessions", map[string]any{})
	if len(list) != 1 || list[0].Name != "pc-one" || list[0].Status != broker.StatusMissing || list[0].AutomaticDeliveryReady || list[0].RegistrationID != first.Session.RegistrationID || list[0].Epoch == first.Session.Epoch {
		t.Fatalf("restored roster: %+v", list)
	}
	resume := broker.ResumeRequest{RegistrationID: first.Session.RegistrationID, Harness: request.Harness, SessionID: request.SessionID, PID: request.PID, CWD: directory, ClientID: request.ClientID, ChannelID: "lease-two", Secret: request.Secret, RequestID: "request-two", ExpectedRevision: first.Session.Revision}
	second := managedTool[broker.RegisterResult](t, h, "resume_session", resume)
	if second.Code != "" || second.Session.Name != "pc-one" || !second.Session.AutomaticDeliveryReady {
		t.Fatalf("resume: %+v", second)
	}
	managedTool[broker.SendResult](t, h, "send_message", map[string]any{"from": "pc-one", "to": "pc-one", "body": "after restart"})
	message := managedTool[broker.Message](t, h, "wait_for_message", map[string]any{"recipient": "pc-one", "channelId": "lease-two"})
	if message.Body != "after restart" {
		t.Fatal("reconnected receiver did not get message")
	}
}

func TestManagedHTTPTokenOwnsNicknameAndMessagingIdentity(t *testing.T) {
	directory := t.TempDir()
	store, err := controlpanel.OpenSQLite(filepath.Join(directory, "control"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Setup("test-only-password-for-runtime"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateProject("test"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProject("test", "token", true, ""); err != nil {
		t.Fatal(err)
	}
	alpha, err := store.IssueToken("test", "ui-alpha")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := store.IssueToken("test", "ui-beta")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	h, close, err := newManagedHandler(directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	bound := func(token string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
			h.ServeHTTP(w, r)
		})
	}
	a, b := bound(alpha), bound(beta)
	for _, client := range []http.Handler{a, b} {
		managedRPC(t, client, "initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "token-test", "version": "1"}}, nil)
	}
	request := broker.RegisterRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: directory, ClientID: "runtime-client-001", Name: "ignored-client-name", ChannelID: "lease-alpha", Secret: strings.Repeat("s", 40), RequestID: "request-alpha"}
	first := managedTool[broker.RegisterResult](t, a, "register_session", request)
	if first.Code != "" || first.Session.Name != "ui-alpha" {
		t.Fatalf("UI admission: %+v", first)
	}
	var denied struct{ IsError bool }
	managedRPC(t, b, "tools/call", map[string]any{"name": "send_message", "arguments": map[string]any{"from": "ui-alpha", "to": "ui-alpha", "body": "forged"}}, &denied)
	if !denied.IsError {
		t.Fatal("unbound token impersonated alpha through HTTP")
	}
	request.ClientID, request.ChannelID, request.RequestID = "runtime-client-002", "lease-beta", "request-beta"
	second := managedTool[broker.RegisterResult](t, b, "register_session", request)
	if second.Code != "" || second.Session.Name != "ui-beta" {
		t.Fatalf("beta admission: %+v", second)
	}
	managedTool[broker.SendResult](t, a, "send_message", map[string]any{"from": "ui-alpha", "to": "ui-beta", "body": "token-authenticated message"})
	got := managedTool[broker.Message](t, b, "wait_for_message", map[string]any{"recipient": "ui-beta", "channelId": "lease-beta"})
	if got.Body != "token-authenticated message" {
		t.Fatal("authorized HTTP delivery failed")
	}
}
