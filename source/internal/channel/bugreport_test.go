package channel

import (
	"context"
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpconfig"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

func TestReportBugRegisteredSessionStoresReporterAndClient(t *testing.T) {
	r := newDetachedRuntime(t)
	h := r.harness(t, t.TempDir(), 9901)
	h.attach(t)
	result, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "report_bug", Arguments: map[string]any{
		"title": "fixture defect", "summary": "tool response differs", "client": map[string]any{"clientVersion": "model-forged", "path": "/private/synthetic-secret"},
	}})
	if err != nil || result.IsError {
		t.Fatal("registered session report_bug is unavailable or refused")
	}
	var response struct {
		ID string `json:"id"`
	}
	data, _ := json.Marshal(result.StructuredContent)
	if json.Unmarshal(data, &response) != nil || response.ID == "" {
		t.Fatal("report confirmation missing")
	}
	db, err := sqlitestore.Open(filepath.Join(r.root, "bugreports"), "bugreports.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	raw, err := db.Get(response.ID)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Reporter struct{ Project, SessionName, RegistrationID, Harness, OwnerID string }
		Client   struct{ ClientVersion, Harness, OS, Arch string }
	}
	if json.Unmarshal(raw, &report) != nil {
		t.Fatal("stored report is invalid")
	}
	if report.Reporter.Project != "detach" || report.Reporter.RegistrationID != h.session.RegistrationID || report.Reporter.SessionName != h.session.Name || report.Reporter.Harness != "codex" || report.Reporter.OwnerID != "admin" {
		t.Fatal("server reporter metadata is incomplete")
	}
	if report.Client.ClientVersion == "" || report.Client.Harness != "codex" || report.Client.OS == "" || report.Client.Arch == "" {
		t.Fatal("client diagnostics missing")
	}
	for _, forbidden := range []string{"model-forged", "/private/synthetic-secret"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatal("model supplied client diagnostics were stored")
		}
	}
}

func TestBugReportDiagnosticsWhitelistAndLastTwenty(t *testing.T) {
	store, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-4111-8111-111111111111"
	key := clientstate.RegistrationKey("private-endpoint", "codex", id)
	now := time.Now().UTC()
	for i := 0; i < 25; i++ {
		if err = store.RecordDiagnostic(key, clientstate.DiagnosticEvent{At: now.Add(time.Duration(i) * time.Second), SessionID: id, RegistrationID: "ABCDEFGHIJKLMNOPQRSTUVWXYZ", Event: "receive-failed", Reason: "network", ErrorClass: "network"}); err != nil {
			t.Fatal(err)
		}
	}
	privateToken := strings.Join([]string{"asb", "s", "syntheticcredential"}, "_")
	b := &ingressBinding{mu: &sync.Mutex{}, started: true, identity: broker.AttachRequest{Harness: "codex", SessionID: id, CWD: "/private/synthetic-path"}, upstreamCredential: privateToken, durable: &durableRegistration{key: key, secret: "private-resume-secret", session: &sessionAuth{status: authAuthorized, state: clientstate.SessionState{Credential: &clientstate.SessionCredential{AccessToken: privateToken}}}}}
	follow := &claudeFollow{result: "followed: handover"}
	opts := durableOptions{state: store, claudeFollow: follow}
	diagnostics := opts.bugDiagnostics(t.Context(), b, unavailableUpstream{})
	if len(diagnostics.Events) != 20 || !diagnostics.Events[0].At.Equal(now.Add(5*time.Second)) || diagnostics.LastFollow != "followed: handover" || !diagnostics.ReceiveReady {
		t.Fatal("diagnostic snapshot incomplete")
	}
	request := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{"title":"fixture","summary":"fixture","client":{"error":"private-raw-error","argv":"private-argv","env":"private-env"}}`)}}
	ctx := context.WithValue(t.Context(), bugDiagnosticsKey{}, diagnostics)
	raw, err := bugReportArguments(ctx, request, unavailableUpstream{})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{privateToken, "private-resume-secret", id, "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "/private/synthetic-path", "private-raw-error", "private-argv", "private-env"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("private diagnostic field escaped whitelist")
		}
	}
	if safeBugVersion("https://private/path") != "unknown" || safeBugVersion(privateToken) != "unknown" {
		t.Fatal("unsafe version included")
	}
	follow.result = "private-raw-error /private/synthetic-path"
	if opts.bugDiagnostics(t.Context(), b, unavailableUpstream{}).LastFollow != "" {
		t.Fatal("unsafe follow text included")
	}
}

func TestReportBugClientListsAndCallsWithoutDiagnostics(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	h := r.harness(t, dir, 9902)
	h.attach(t)
	state, err := clientstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	clientID, err := state.ClientID()
	if err != nil {
		t.Fatal(err)
	}
	key := clientstate.SessionKey(brokerScope(r.endpoint, "detach"), "codex", "11111111-1111-4111-8111-000000009902", clientID)
	saved, found, err := state.LoadSession(key)
	if err != nil || !found {
		t.Fatal("fixture credential unavailable")
	}
	ctx := mcpconfig.WithVersion(t.Context(), "1.0.0")
	upstream := newTestBrokerUpstream(ctx, r.endpoint)
	defer upstream.Close()
	upstream.setBearerToken(saved.Credential.AccessToken)
	// Forward the live server catalog and raw arguments without diagnostics. It has
	// no new client interception and therefore attaches no client diagnostics.
	session, err := upstream.session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTool(listed.Tools, "report_bug") {
		t.Fatal("old client catalog omitted report_bug")
	}
	result, err := upstream.CallTool(ctx, &mcp.CallToolParams{Name: "report_bug", Arguments: map[string]any{"title": "old client", "summary": "no diagnostic payload"}})
	if err != nil || result.IsError {
		t.Fatal("old client report refused")
	}
	var response struct {
		ID string `json:"id"`
	}
	raw, _ := json.Marshal(result.StructuredContent)
	if json.Unmarshal(raw, &response) != nil || response.ID == "" {
		t.Fatal("old client confirmation missing")
	}
	db, err := sqlitestore.Open(filepath.Join(r.root, "bugreports"), "bugreports.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	raw, err = db.Get(response.ID)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Client any `json:"client"`
	}
	if json.Unmarshal(raw, &report) != nil || report.Client != nil {
		t.Fatal("old client diagnostics invented")
	}
}
