package controlpanel

import (
	"database/sql"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBugReportDeleteAdminPost(t *testing.T) {
	s, b, h := misuseFixture(t)
	fixture := bugReportFixture()
	fixture.Reporter.ProjectGenerationID = s.ProjectGeneration("demo")
	fixture.Summary = "DELETE-BODY-SENTINEL"
	saved, err := b.reports.Create(fixture.Input, fixture.Reporter, "test")
	if err != nil {
		t.Fatal(err)
	}
	c := loginPanelUser(t, h, "admin")
	w := c.call("POST", "/admin/bug-reports/delete?status=all&severity=critical&page=2", url.Values{"id": {saved.ID}, "expectedVersion": {saved.ExpectedVersion}, "confirm": {"delete"}})
	if w.Code != 303 {
		t.Fatalf("delete POST=%d, want303", w.Code)
	}
	if body := c.call("GET", "/admin/bug-reports?status=all", nil).Body.String(); strings.Contains(body, saved.ID) {
		t.Fatal("deleted report still listed")
	}
	d, err := b.reports.DeletionFor("admin", saved.ID)
	if err != nil || d.Phase != "deleted" {
		t.Fatal(d, err)
	}
	if w := c.call("GET", "/admin/bug-reports/detail?id="+saved.ID, nil); w.Code != 410 || strings.Contains(w.Body.String(), "DELETE-BODY-SENTINEL") {
		t.Fatal("deleted report detail", w.Code)
	}
}

func deletePanelFixture(t *testing.T) (*Store, *misuseBackend, *userPanelClient, string, bugreport.Report) {
	t.Helper()
	s, b, h := misuseFixture(t)
	root := t.TempDir()
	reports, err := bugreport.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reports.Close() })
	reports.SetProjectPolicy(s.BugReportPolicy)
	b.reports = reports
	fixture := bugReportFixture()
	fixture.Reporter.ProjectGenerationID = s.ProjectGeneration("demo")
	fixture.Title = "DELETE-SENTINEL"
	fixture.Summary = "DELETE-SENTINEL"
	r, err := reports.Create(fixture.Input, fixture.Reporter, "test")
	if err != nil {
		t.Fatal(err)
	}
	return s, b, loginPanelUser(t, h, "admin"), root, r
}
func deleteForm(r bugreport.Report) url.Values {
	return url.Values{"id": {r.ID}, "expectedVersion": {r.ExpectedVersion}, "confirm": {"delete"}}
}
func deleteSQL(t *testing.T, root, file, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, "bugreports", file))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(statement); err != nil {
		t.Fatal(err)
	}
}
func TestBugReportDeleteFailuresAndRetry(t *testing.T) {
	for _, mode := range []string{"retry", "prune", "complete", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			_, b, c, root, r := deletePanelFixture(t)
			if mode == "unavailable" {
				if err := os.Mkdir(filepath.Join(root, "bugreports", "deletions.db"), 0700); err != nil {
					t.Fatal(err)
				}
			} else if mode == "complete" {
				_, _ = b.reports.DeletionFor("admin", r.ID)
				deleteSQL(t, root, "deletions.db", `CREATE TRIGGER fail_complete BEFORE UPDATE ON records WHEN json_extract(NEW.payload,'$.phase')='deleted' BEGIN SELECT RAISE(ABORT,'injected complete failure'); END`)
			} else {
				deleteSQL(t, root, "bugreports.db", `CREATE TRIGGER fail_delete BEFORE DELETE ON records BEGIN SELECT RAISE(ABORT,'injected delete failure'); END`)
			}
			w := c.call("POST", "/admin/bug-reports/delete?status=all", deleteForm(r))
			if mode == "complete" {
				if w.Code != 303 {
					t.Fatal("incomplete redirect", w.Code)
				}
				req := httptest.NewRequest("GET", "http://broker"+w.Header().Get("Location"), nil)
				req.AddCookie(c.cookie)
				for _, cookie := range w.Result().Cookies() {
					req.AddCookie(cookie)
				}
				next := httptest.NewRecorder()
				c.h.ServeHTTP(next, req)
				if next.Code != 200 || !strings.Contains(next.Body.String(), en("notice.bug-report-delete-incomplete")) || !strings.Contains(next.Body.String(), `class="notice error"`) {
					t.Fatal("incomplete notice", next.Code)
				}
				d, err := b.reports.DeletionFor("admin", r.ID)
				if err != nil || d.Phase != "requested" {
					t.Fatal(d, err)
				}
				detail := c.call("GET", "/admin/bug-reports/detail?id="+r.ID, nil)
				if detail.Code != 404 || !strings.Contains(detail.Body.String(), en("bug.not_found")) {
					t.Fatal("false410", detail.Code)
				}
				return
			}
			if w.Code != 503 {
				t.Fatal("failed delete", w.Code)
			}
			if w := c.call("GET", "/admin/bug-reports/detail?id="+r.ID, nil); w.Code != 200 {
				t.Fatal("report disappeared", w.Code)
			}
			if mode == "unavailable" {
				return
			}
			d, err := b.reports.DeletionFor("admin", r.ID)
			if err != nil || d.Phase != "requested" {
				t.Fatal(d, err)
			}
			deleteSQL(t, root, "bugreports.db", `DROP TRIGGER fail_delete`)
			if mode == "prune" {
				// Retention removes content independently of an administrator's failed request.
				deleteSQL(t, root, "bugreports.db", `UPDATE records SET payload=json_set(CAST(payload AS TEXT),'$.receivedAt','2000-01-01T00:00:00Z') WHERE key='`+r.ID+`'`)
				fixture := bugReportFixture()
				fixture.Reporter.ProjectGenerationID = r.Reporter.ProjectGenerationID
				if _, err := b.reports.Create(fixture.Input, fixture.Reporter, "test"); err != nil {
					t.Fatal(err)
				}
				detail := c.call("GET", "/admin/bug-reports/detail?id="+r.ID, nil)
				if detail.Code != 404 || !strings.Contains(detail.Body.String(), en("bug.not_found")) {
					t.Fatal("pruning became admin deletion", detail.Code)
				}
			} else {
				if w := c.call("POST", "/admin/bug-reports/delete", deleteForm(r)); w.Code != 303 {
					t.Fatal(w.Code)
				}
				d, err = b.reports.DeletionFor("admin", r.ID)
				if err != nil || d.Phase != "deleted" {
					t.Fatal(d, err)
				}
				if w := c.call("POST", "/admin/bug-reports/delete", deleteForm(r)); w.Code != 410 {
					t.Fatal("duplicate delete", w.Code)
				}
			}
		})
	}
}
func TestBugReportDeleteConflictAndFormBoundaries(t *testing.T) {
	_, b, c, _, r := deletePanelFixture(t)
	stale := deleteForm(r)
	stale.Set("expectedVersion", "stale")
	w := c.call("POST", "/admin/bug-reports/delete", stale)
	if w.Code != 409 || !strings.Contains(w.Body.String(), enHTML("bug.delete_conflict")) || !strings.Contains(w.Body.String(), `aria-live="assertive"`) || strings.Contains(w.Body.String(), `value="delete" checked`) {
		t.Fatal("conflict not rendered", w.Code)
	}
	if _, err := b.reports.DeletionFor("admin", r.ID); !errors.Is(err, bugreport.ErrNotFound) {
		t.Fatal("conflict side effect", err)
	}
	for _, key := range []string{"id", "expectedVersion", "confirm"} {
		for _, mode := range []string{"missing", "duplicate", "empty"} {
			f := deleteForm(r)
			switch mode {
			case "missing":
				f.Del(key)
			case "duplicate":
				f.Add(key, f.Get(key))
			case "empty":
				f.Set(key, "")
			}
			if w := c.call("POST", "/admin/bug-reports/delete", f); w.Code != 400 {
				t.Fatal(key, mode, w.Code)
			}
		}
	}
	f := deleteForm(r)
	f.Set("unknown", "value")
	if w := c.call("POST", "/admin/bug-reports/delete", f); w.Code != 400 {
		t.Fatal(w.Code)
	}
	f = deleteForm(r)
	f.Set("confirm", "yes")
	if w := c.call("POST", "/admin/bug-reports/delete", f); w.Code != 400 {
		t.Fatal(w.Code)
	}
	for _, csrf := range [][]string{nil, {"wrong"}, {c.csrf, c.csrf}} {
		f = deleteForm(r)
		f["csrf"] = csrf
		req := httptest.NewRequest("POST", "http://broker/admin/bug-reports/delete", strings.NewReader(f.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(c.cookie)
		w := httptest.NewRecorder()
		c.h.ServeHTTP(w, req)
		want := 403
		if len(csrf) > 1 {
			want = 400
		}
		if w.Code != want {
			t.Fatal("csrf", w.Code)
		}
	}
	if w := c.call("GET", "/admin/bug-reports/delete?id="+r.ID, nil); w.Code != 405 {
		t.Fatal(w.Code)
	}
	outsider := loginPanelUser(t, c.h, "outsider")
	if w := outsider.call("POST", "/admin/bug-reports/delete", deleteForm(r)); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if _, err := b.reports.Get(r.ID); err != nil {
		t.Fatal("invalid request deleted report", err)
	}
}
func TestBugReportDeleteOldGenerationAndExactID(t *testing.T) {
	s, b, c, _, r := deletePanelFixture(t)
	if err := s.CreateProjectFor("admin", "another"); err != nil {
		t.Fatal(err)
	}
	// Change the policy generation while retaining the old report.
	b.reports.SetProjectPolicy(func(actor string) (map[string]string, func(), error) {
		if actor != "admin" && actor != "" {
			return nil, nil, bugreport.ErrForbidden
		}
		return map[string]string{"demo": "new-generation"}, func() {}, nil
	})
	detail := c.call("GET", "/admin/bug-reports/detail?bucket=previous&id="+r.ID, nil)
	if detail.Code != 200 || !strings.Contains(detail.Body.String(), `action="/admin/bug-reports/delete`) {
		t.Fatal("old generation delete form", detail.Code)
	}
	// A deliberately mismatched display bucket cannot redirect the deletion target.
	if w := c.call("POST", "/admin/bug-reports/delete?bucket=unknown&status=all&page=2", deleteForm(r)); w.Code != 303 || !strings.Contains(w.Header().Get("Location"), "bucket=unknown") {
		t.Fatal("bucket selected deletion target", w.Code)
	}
}

func TestBugReportDeleteConcurrentRequests(t *testing.T) {
	_, _, c, _, r := deletePanelFixture(t)
	codes := make(chan int, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() { <-start; codes <- c.call("POST", "/admin/bug-reports/delete", deleteForm(r)).Code }()
	}
	close(start)
	a, b := <-codes, <-codes
	if !(a == 303 && b == 410 || a == 410 && b == 303) {
		t.Fatal("concurrent delete", a, b)
	}
}
