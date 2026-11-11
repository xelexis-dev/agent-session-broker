package controlpanel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

// Full Chrome uses isolated synthetic records and a private, ephemeral cookie file.
func TestBugReportFullChrome(t *testing.T) {
	if os.Getenv("ASB_BUG_REPORT_BROWSER") == "" {
		t.Skip("opt-in full Chrome")
	}
	s := userStore(t)
	useKorean(t, s)
	dir := t.TempDir()
	db, err := sqlitestore.Open(filepath.Join(dir, "bugreports"), "bugreports.db")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for i, state := range []string{"open", "resolved", "ignored", "closed", "previous", "long"} {
		r := bugReportFixture()
		r.ID = "bug-" + state
		r.Reporter.ProjectGenerationID = s.ProjectGeneration("demo")
		r.ReceivedAt = time.Now().UTC().Add(-time.Duration(i) * time.Minute)
		r.Severity = []string{"critical", "high", "medium", "low"}[i%4]
		if state == "resolved" || state == "ignored" {
			r.Status = "closed"
			r.Resolution = state
			r.Note = "수정 배포 확인"
			r.StatusChangedAt = r.ReceivedAt
			r.StatusChangedBy = "admin"
			r.StatusChanges = []bugreport.StatusChange{{At: r.ReceivedAt, From: "open", To: state, Actor: "admin", Source: "mcp", Session: &bugreport.ChangeSession{Name: "lead-agent", RegistrationID: "fixture-reg"}, Note: r.Note}}
		}
		if state == "closed" {
			r.Status = "closed"
		}
		if state == "previous" {
			r.Reporter.ProjectGenerationID = "previous-generation"
		}
		if state == "long" {
			r.Title = strings.Repeat("긴 제목과 보고 내용 확인 ", 10)
			r.Note = strings.Repeat("처리 근거 ", 70)
			r.HistoryTruncated = true
			r.Summary = strings.Repeat("긴 보고 내용입니다. ", 30)
		}
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err = db.Put(r.ID, raw); err != nil {
			t.Fatal(err)
		}
		ids[state] = r.ID
	}
	db.Close()
	reports, err := bugreport.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reports.Close()
	reports.SetProjectPolicy(s.BugReportPolicy)
	h := New(s, &reportPanelBackend{reports: reports}, http.NotFoundHandler(), "0.17.3")
	server := httptest.NewServer(h)
	defer server.Close()
	client := loginPanelUser(t, h, "admin")
	type afterAuth struct{ Base, CookieName, Cookie, ReportID string }
	after := map[string]afterAuth{}
	// Each after-change capture gets an independent store, so its transition
	// cannot change the normal-render fixtures or another theme's history.
	for _, key := range []string{"1280-light", "1280-dark", "390-light", "390-dark"} {
		control := userStore(t)
		useKorean(t, control)
		store, err := bugreport.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		fixture := bugReportFixture()
		fixture.Reporter.ProjectGenerationID = control.ProjectGeneration("demo")
		report, err := store.Create(fixture.Input, fixture.Reporter, "0.17.3")
		if err != nil {
			t.Fatal(err)
		}
		store.SetProjectPolicy(control.BugReportPolicy)
		handler := New(control, &reportPanelBackend{reports: store}, http.NotFoundHandler(), "0.17.3")
		captureServer := httptest.NewServer(handler)
		t.Cleanup(captureServer.Close)
		login := loginPanelUser(t, handler, "admin")
		after[key] = afterAuth{captureServer.URL, login.cookie.Name, login.cookie.Value, report.ID}
	}
	config := struct {
		Base, CookieName, Cookie string
		IDs                      map[string]string
		After                    map[string]afterAuth
	}{server.URL, client.cookie.Name, client.cookie.Value, ids, after}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(t.TempDir(), "browser-private.json")
	if os.WriteFile(private, data, 0600) != nil {
		t.Fatal("private browser config")
	}
	cmd := exec.CommandContext(t.Context(), "node", "bug_reports_browser.cjs", private)
	if err = cmd.Run(); err != nil {
		t.Fatal("full Chrome assertions failed; inspect sanitized evidence directory")
	}
}
