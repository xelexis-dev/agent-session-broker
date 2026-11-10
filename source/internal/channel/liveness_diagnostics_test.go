package channel

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
)

type failedRenewUpstream struct{ Upstream }

func (u failedRenewUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name == "renew_lease" {
		return nil, errors.New("https://secret.invalid/?token=diagnostic-test-secret Authorization: Bearer diagnostic-test-secret argv message-body")
	}
	return u.Upstream.CallTool(ctx, p)
}

func TestRenewFailureWritesSafeDiagnostic(t *testing.T) {
	u := newSwitchableUpstream(t)
	request := broker.RegisterRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 4242, CWD: "/work", ClientID: "client-diagnostics", Name: "diagnostics", ChannelID: "lease-1", Secret: strings.Repeat("s", 40), RequestID: "req-1"}
	first, err := u.service.RegisterSession(t.Context(), request)
	if err != nil || first.Code != "" {
		t.Fatalf("register: %s %v", first.Code, err)
	}
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := clientstate.RegistrationKey("scope", "codex", request.SessionID)
	b := &ingressBinding{mu: &sync.Mutex{}, identity: broker.AttachRequest{Harness: request.Harness, SessionID: request.SessionID, Name: request.Name, PID: request.PID, CWD: request.CWD, ChannelID: request.ChannelID}, generation: first.Session.Generation, durable: &durableRegistration{key: key, registrationID: first.Session.RegistrationID}}
	before := time.Now().UTC()
	exit := receiveDurableOnce(t.Context(), failedRenewUpstream{u}, b, durableOptions{state: state, heartbeat: time.Millisecond, timeout: time.Second}, func(context.Context, broker.Message) error { return nil })
	if exit != exitUpstream {
		t.Fatal("unexpected receiver exit", exit)
	}
	raw, err := os.ReadFile(filepath.Join(state.Dir(), "diagnostics", "v2", key+".json"))
	if err != nil {
		t.Fatalf("renew failure left no diagnostic record: %v", err)
	}
	for _, secret := range []string{"diagnostic-test-secret", "secret.invalid", "Authorization", "argv", "message-body"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("diagnostic leaked error payload")
		}
	}
	var snapshot clientstate.Diagnostics
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Events) != 1 || snapshot.Events[0].At.Before(before) || snapshot.Events[0].At.After(time.Now().UTC()) || snapshot.Events[0].Generation != first.Session.Generation || snapshot.Summary.LastFailureReason != "renew-failed" {
		t.Fatalf("incorrect safe failure snapshot: %+v", snapshot)
	}
	if !strings.Contains(string(raw), "renew-failed") || !strings.Contains(string(raw), first.Session.RegistrationID) || !strings.Contains(string(raw), before.Format("2006-01-02")) {
		t.Fatal("missing reason, registration ID or timestamp")
	}
}

func loadFixtureDiagnostics(t *testing.T, dir, scope, harness string, device bool) clientstate.Diagnostics {
	t.Helper()
	s, err := clientstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := clientstate.RegistrationKey(scope, harness, "11111111-1111-4111-8111-111111111111")
	if device {
		id, err := s.ClientID()
		if err != nil {
			t.Fatal(err)
		}
		key = clientstate.SessionKey(scope, harness, "11111111-1111-4111-8111-111111111111", id)
	}
	out, err := s.LoadDiagnostics(key)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func hasDiagnostic(out clientstate.Diagnostics, event, reason string) bool {
	for _, e := range out.Events {
		if e.Event == event && (reason == "" || e.Reason == reason || e.AuthState == reason) {
			return true
		}
	}
	return false
}

func TestLivenessDiagnosticsRecordRecoveryAndReceiverExit(t *testing.T) {
	u := newSwitchableUpstream(t)
	f := newDurableFixture(t, u, t.TempDir())
	first, result := f.attach(t, "diagnostics")
	if result.IsError {
		t.Fatal("attach failed")
	}
	snapshot := func() clientstate.Diagnostics {
		return loadFixtureDiagnostics(t, f.state.Dir(), "http://broker/mcp?project=brk", "claude", false)
	}
	waitFor(t, "successful renewal", func() bool { return !snapshot().Summary.LastRenewSuccess.IsZero() })
	u.offline.Store(true)
	waitFor(t, "failed recovery", func() bool { return hasDiagnostic(snapshot(), "reconnect-failed", "") })
	u.offline.Store(false)
	waitFor(t, "recovered receiver", func() bool { return hasDiagnostic(snapshot(), "reconnected", "") })
	f.stop()
	out := snapshot()
	for _, pair := range [][2]string{{"renew-failed", ""}, {"receiver-exit", "upstream"}, {"reconnect-start", ""}, {"reconnect-failed", ""}, {"reconnected", ""}, {"receiver-exit", "context"}} {
		if !hasDiagnostic(out, pair[0], pair[1]) {
			t.Fatalf("missing diagnostic %s/%s", pair[0], pair[1])
		}
	}
	for _, e := range out.Events {
		if e.SessionID != f.identity.SessionID || e.RegistrationID != first.RegistrationID || e.Generation == 0 || e.At.IsZero() {
			t.Fatal("lost binding identity or time")
		}
	}
}

func TestLivenessDiagnosticsAuthStatusAndTransitions(t *testing.T) {
	auth, u := newDeviceAuthServer(t), newSwitchableUpstream(t)
	dir := t.TempDir()
	f := newDeviceModeFixture(t, auth, u, dir, singleIdentity())
	waitFor(t, "pending authorization", func() bool { return f.status("")["status"] == "pending" })
	f.grant("11111111-1111-4111-8111-111111111111", fixtureToken("asb_s_"), "diagnostics")
	waitFor(t, "auth status renewal summary", func() bool {
		status := f.status("")
		summary, ok := status["diagnostics"].(map[string]any)
		return status["registered"] == true && ok && summary["lastRenewSuccess"] != nil
	})
	f.mu.Lock()
	for _, upstream := range f.upstreams {
		upstream.rejected.Store(true)
	}
	f.mu.Unlock()
	waitFor(t, "rejected authorization", func() bool { return f.status("")["status"] == "rejected" })
	summary := f.status("")["diagnostics"].(map[string]any)
	if summary["authState"] != "rejected" || summary["lastFailureClass"] != "authorization" || summary["lastFailureAt"] == nil || summary["lastRenewSuccess"] == nil {
		t.Fatal("auth_status omitted persisted failure/success summary")
	}
	out := loadFixtureDiagnostics(t, dir, "https://broker/mcp?project=brk", "claude", true)
	for _, status := range []string{"unauthorized", "pending", "authorized", "rejected"} {
		if !hasDiagnostic(out, "auth-transition", status) {
			t.Fatal("missing authorization transition", status)
		}
	}
	if !hasDiagnostic(out, "transport-created", "") {
		t.Fatal("transport creation not recorded")
	}
	// Explicit approval creates a replacement transport, without reusing the rejected credential.
	f.call("", "attach_session", map[string]any{})
	f.grant("11111111-1111-4111-8111-111111111111", fixtureToken("asb_s_"), "diagnostics")
	waitFor(t, "replacement authorization", func() bool { return f.status("")["registered"] == true })
	out = loadFixtureDiagnostics(t, dir, "https://broker/mcp?project=brk", "claude", true)
	if !hasDiagnostic(out, "transport-replaced", "") {
		t.Fatal("transport replacement not recorded")
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "asb_s_") || strings.Contains(string(raw), "diagnostic-fixture") {
		t.Fatal("credential leaked in diagnostics")
	}
}

func TestLivenessDiagnosticsUnavailableDoesNotStopRenewal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "diagnostics"), []byte("unavailable"), 0600); err != nil {
		t.Fatal(err)
	}
	auth, u := newDeviceAuthServer(t), newSwitchableUpstream(t)
	f := newDeviceModeFixture(t, auth, u, dir, singleIdentity())
	f.grant("11111111-1111-4111-8111-111111111111", "asb_s_fixture", "diagnostics")
	waitFor(t, "registered without diagnostic storage", func() bool { return f.status("")["registered"] == true && f.log.last("renew_lease") != "" })
	summary := f.status("")["diagnostics"].(map[string]any)
	if summary["unavailable"] != true {
		t.Fatal("unavailable diagnostic storage was hidden")
	}
	session, err := u.session("diagnostics")
	if err != nil || session.Status != broker.StatusAlive {
		t.Fatal("diagnostic storage error interrupted liveness")
	}
}

func TestDetachedRenewRecordsStopAndGuidance(t *testing.T) {
	b := &ingressBinding{mu: &sync.Mutex{}}
	o := durableOptions{}
	o.diagnoseRenew(b, nil, broker.RegisterResult{Code: broker.CodeDetached}, nil)
	if b.resumeStopReason != "registration-detached" || !strings.Contains(autoRecoveryGuidance(b.resumeStopReason), detachedGuidance) {
		t.Fatal("detached renew does not explain explicit user reconnect")
	}
}
