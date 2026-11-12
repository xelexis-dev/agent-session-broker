package serverruntime

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
)

func TestBugReportCorruptStoreDegradesAndRecoversAfterRestore(t *testing.T) {
	r, parent := newDeviceRuntime(t)
	request := protectedRequest(961, "reporter")
	credential := r.authorize(parent, request, "reporter")
	registered := managedTool[broker.RegisterResult](t, r.bound(credential), "register_session", request)
	before := managedTool[struct{ ID string }](t, r.bound(credential), "report_bug", bugreport.Input{Title: "saved report", Summary: "fixture"})
	if r.close() != nil {
		t.Fatal("fixture close failed")
	}
	path := filepath.Join(r.dir, "bugreports", "bugreports.db")
	backup, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("backup fixture failed")
	}
	damaged := []byte("synthetic damaged report database")
	if os.WriteFile(path, damaged, 0600) != nil {
		t.Fatal("damage fixture failed")
	}
	identity, _ := os.Stat(path)
	handler, closeHandler, err := newManagedHandler(r.dir, nil)
	if err != nil {
		t.Fatal("corrupt bugreports.db prevented server startup")
	}
	r.h, r.close = handler, closeHandler
	r.login(r.password)
	// The healthy registration still resumes while only reporting is unavailable.
	resumed := managedTool[broker.RegisterResult](t, r.bound(credential), "resume_session", broker.ResumeRequest{RegistrationID: registered.Session.RegistrationID, Harness: request.Harness, SessionID: request.SessionID, PID: request.PID, CWD: request.CWD, ClientID: request.ClientID, Secret: request.Secret, ChannelID: "new-lease", RequestID: "resume", ExpectedRevision: registered.Session.Revision})
	if resumed.Code != "" {
		t.Fatal("healthy registration unavailable")
	}
	var failed struct {
		IsError bool
		Content []struct{ Text string }
	}
	managedRPC(t, r.bound(credential), "tools/call", map[string]any{"name": "report_bug", "arguments": bugreport.Input{Title: "new report", Summary: "fixture"}}, &failed)
	if !failed.IsError || len(failed.Content) != 1 || failed.Content[0].Text != "bug report storage is unavailable" {
		t.Fatal("report_bug did not return fixed unavailable error")
	}
	for _, endpoint := range []string{"/admin/bug-reports", "/admin/bug-reports/detail?id=" + before.ID} {
		out := r.browser(http.MethodGet, endpoint, nil)
		if out.Code != http.StatusServiceUnavailable || !strings.Contains(out.Body.String(), "bug report storage is unavailable") {
			t.Fatal("admin GET unavailable state missing")
		}
	}
	out := r.browser(http.MethodPost, "/admin/bug-reports/status", url.Values{"id": {before.ID}, "status": {"resolved"}, "expectedVersion": {"fixture-version"}})
	if out.Code != 503 || !strings.Contains(out.Body.String(), "bug report storage is unavailable") {
		t.Fatal("admin status unavailable state missing")
	}
	warning := captureDiagnostics(t)
	out = r.browser(http.MethodPost, "/projects/delete", url.Values{"project": {"test"}, "confirmName": {"test"}})
	if out.Code != 303 {
		t.Fatal("project deletion blocked by unavailable report store")
	}
	if !strings.Contains(warning.String(), "warning: project deletion skipped unavailable bug report storage") || strings.Contains(warning.String(), r.dir) {
		t.Fatal("fixed deletion warning missing or leaked a path")
	}

	if r.close() != nil {
		t.Fatal("degraded close failed")
	}
	after, _ := os.ReadFile(path)
	afterIdentity, _ := os.Stat(path)
	if !bytes.Equal(after, damaged) || !os.SameFile(identity, afterIdentity) {
		t.Fatal("degraded runtime changed corrupt file")
	}
	// Only the operator fixture restores the file; the runtime never does so.
	if os.WriteFile(path, backup, 0600) != nil {
		t.Fatal("operator restore fixture failed")
	}
	r.start()
	r.login(r.password)
	out = r.browser(http.MethodGet, "/admin/bug-reports", nil)
	if out.Code != 200 || strings.Contains(out.Body.String(), before.ID) || strings.Contains(out.Body.String(), "saved report") {
		t.Fatal("restored store unavailable or orphan report shown")
	}
	out = r.browser(http.MethodGet, "/admin/bug-reports/detail?id="+before.ID, nil)
	if out.Code != 404 {
		t.Fatal("orphan report detail not hidden")
	}
}

// Opt-in capture uses the real degraded managed server and admin handler.
func TestBugReportUnavailableFullChrome(t *testing.T) {
	script := os.Getenv("ASB_DEGRADED_BROWSER_SCRIPT")
	if script == "" {
		t.Skip("opt-in full Chrome")
	}
	r, _ := newDeviceRuntime(t)
	if r.close() != nil {
		t.Fatal("fixture close failed")
	}
	if os.WriteFile(filepath.Join(r.dir, "bugreports", "bugreports.db"), []byte("synthetic corrupt report database"), 0600) != nil {
		t.Fatal("fixture corruption failed")
	}
	r.start()
	r.login(r.password)
	server := httptest.NewServer(r.h)
	defer server.Close()
	config := struct{ Base, CookieName, Cookie string }{server.URL, r.cookie.Name, r.cookie.Value}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal("browser config failed")
	}
	private := filepath.Join(t.TempDir(), "browser-private.json")
	if os.WriteFile(private, data, 0600) != nil {
		t.Fatal("browser config failed")
	}
	cmd := exec.CommandContext(t.Context(), "node", script, private)
	if cmd.Run() != nil {
		t.Fatal("full Chrome failed; inspect sanitized evidence status")
	}
}
