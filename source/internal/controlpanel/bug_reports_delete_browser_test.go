package controlpanel

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

func TestBugReportDeleteFullChrome(t *testing.T) {
	if os.Getenv("ASB_BUG_DELETE_BROWSER") == "" {
		t.Skip("opt-in full Chrome")
	}
	type auth struct{ Base, CookieName, Cookie, ReportID, LongID string }
	config := map[string]auth{}
	for _, width := range []int{1280, 390} {
		for _, theme := range []string{"light", "dark"} {
			for _, state := range []string{"success", "not-confirmed", "card", "conflict", "deleted", "requested"} {
				control := userStore(t)
				useKorean(t, control)
				dir := t.TempDir()
				db, err := sqlitestore.Open(filepath.Join(dir, "bugreports"), "bugreports.db")
				if err != nil {
					t.Fatal(err)
				}
				target := "bug-7Q2MZXK4TD3RRQJ5WNPLB6HVYA"
				for i := 0; i < 5; i++ {
					r := bugReportFixture()
					r.ID = fmt.Sprintf("bug-demo-%03d", i)
					if i == 0 {
						r.ID = target
					}
					r.Reporter.ProjectGenerationID = control.ProjectGeneration("demo")
					raw, _ := json.Marshal(r)
					if err := db.Put(r.ID, raw); err != nil {
						t.Fatal(err)
					}
				}
				db.Close()
				reports, err := bugreport.Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { reports.Close() })
				reports.SetProjectPolicy(control.BugReportPolicy)
				if state == "not-confirmed" || state == "requested" {
					_, _ = reports.DeletionFor("admin", target)
					deleteSQL(t, dir, "deletions.db", `CREATE TRIGGER fail_complete BEFORE UPDATE ON records WHEN json_extract(NEW.payload,'$.phase')='deleted' BEGIN SELECT RAISE(ABORT,'injected browser fixture'); END`)
				}
				var longID string
				if state == "deleted" || state == "requested" {
					r, err := reports.Get(target)
					if err != nil {
						t.Fatal(err)
					}
					d, err := reports.Delete("admin", target, r.ExpectedVersion)
					if err != nil && !(state == "requested" && errors.Is(err, bugreport.ErrDeleteIncomplete)) {
						t.Fatal(err)
					}
					if state == "deleted" {
						longID = "bug-" + strings.Repeat("LONG-ID-", 30)
						d.ReportID = longID
						raw, _ := json.Marshal(d)
						records, err := sqlitestore.Open(filepath.Join(dir, "bugreports"), "deletions.db")
						if err != nil {
							t.Fatal(err)
						}
						if err = records.Put(longID, raw); err != nil {
							t.Fatal(err)
						}
						records.Close()
					}
				}
				handler := New(control, &reportPanelBackend{reports: reports}, http.NotFoundHandler(), "0.17.3")
				server := httptest.NewServer(handler)
				t.Cleanup(server.Close)
				login := loginPanelUser(t, handler, "admin")
				config[fmt.Sprintf("%d-%s-%s", width, theme, state)] = auth{server.URL, login.cookie.Name, login.cookie.Value, target, longID}
			}
		}
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(t.TempDir(), "browser-private.json")
	if os.WriteFile(private, data, 0600) != nil {
		t.Fatal("private browser config")
	}
	cmd := exec.CommandContext(t.Context(), "node", "bug_reports_delete_browser.cjs", private)
	if err = cmd.Run(); err != nil {
		t.Fatal("full Chrome deletion assertions failed; inspect sanitized evidence directory")
	}
}
