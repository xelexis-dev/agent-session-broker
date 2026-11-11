package controlpanel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

// Full Chrome: the admin pages and session approval in English and Korean (see admin_language_browser.cjs).
func TestAdminLanguageBrowserFullChrome(t *testing.T) {
	if os.Getenv("ASB_ADMIN_LANGUAGE_BROWSER") == "" {
		t.Skip("opt-in admin language full Chrome")
	}
	s := userStore(t)
	if err := s.CreateUser("admin", "admin-ko", testPassword, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateLanguage("admin-ko", "ko"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	db, err := sqlitestore.Open(filepath.Join(dir, "bugreports"), "bugreports.db")
	if err != nil {
		t.Fatal(err)
	}
	report := englishBugReport()
	report.Reporter.ProjectGenerationID = s.ProjectGeneration("demo")
	report.ReceivedAt = time.Now().UTC()
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Put(report.ID, raw); err != nil {
		t.Fatal(err)
	}
	db.Close()
	reports, err := bugreport.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reports.Close()
	reports.SetProjectPolicy(s.BugReportPolicy)
	server := httptest.NewServer(New(s, &reportPanelBackend{reports: reports}, http.NotFoundHandler(), "0.17.3"))
	defer server.Close()
	data, _ := json.Marshal(map[string]string{"base": server.URL, "password": testPassword, "report": report.ID})
	config := filepath.Join(t.TempDir(), "admin-language-browser.json")
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := exec.CommandContext(t.Context(), "node", "admin_language_browser.cjs", config).Run(); err != nil {
		t.Fatal("admin language browser/axe check failed; inspect sanitized evidence")
	}
}
