package controlpanel

import (
	"encoding/json"
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGenerationHistoryAdminAuditUnknown(t *testing.T) {
	s := userStore(t)
	s.mu.Lock()
	s.audits["history-unknown"] = AuditEvent{Project: "demo", Kind: "handover", At: time.Now().UTC(), Handover: &protocol.HandoverAudit{Name: "historic-worker", RegistrationID: "historic-registration"}}
	s.mu.Unlock()
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	c := loginPanelUser(t, h, "admin")
	w := c.call("GET", "/projects/audit?project=demo&bucket=history", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "historic-worker") || !strings.Contains(w.Body.String(), "이전 기록(세대 불명)") {
		t.Fatalf("admin cannot view unknown audit separately: status=%d", w.Code)
	}
}

// Seed persisted records as if restored from an older backup. No live server or
// production data is involved; the actual store performs all bucket filtering.
func generationHistoryFixture(t *testing.T, kinds []string) (*Store, *bugreport.Store, http.Handler) {
	t.Helper()
	s := userStore(t)
	if err := s.SetScopeIssuanceReady("admin", true); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	db, err := sqlitestore.Open(filepath.Join(dir, "bugreports"), "bugreports.db")
	if err != nil {
		t.Fatal(err)
	}
	for i, kind := range kinds {
		generation := ""
		if kind == "current" {
			generation = s.ProjectGeneration("demo")
		}
		if kind == "previous" {
			generation = "old-generation"
		}
		r := bugReportFixture()
		r.ID = fmt.Sprintf("bug-%s-%03d", kind, i)
		r.Title = "보고 내용 " + kind
		r.ReceivedAt = time.Now().UTC().Add(-time.Duration(i) * time.Second)
		r.Reporter.ProjectGenerationID = generation
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err = db.Put(r.ID, raw); err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		s.audits[r.ID] = AuditEvent{Version: 1, Project: "demo", ProjectGenerationID: generation, OwnerID: "owner", Kind: "handover", At: r.ReceivedAt, Handover: &protocol.HandoverAudit{Name: "worker-" + kind, RegistrationID: "reg-" + kind, At: r.ReceivedAt, Previous: strings.Repeat("a", 64), Current: strings.Repeat("b", 64)}}
		s.mu.Unlock()
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reports, err := bugreport.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reports.Close() })
	reports.SetProjectPolicy(s.BugReportPolicy)
	h := New(s, &reportPanelBackend{reports: reports}, http.NotFoundHandler(), "0.17.2")
	return s, reports, h
}

func TestGenerationHistoryNoticesAndCurrentIsolation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kinds  []string
		notice string
	}{
		{"unknown", []string{"unknown"}, historyUnknownNotice},
		{"previous", []string{"previous"}, historyPreviousNotice},
		{"mixed", []string{"unknown", "previous"}, historyMixedNotice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kinds := append([]string{"current"}, tc.kinds...)
			_, _, h := generationHistoryFixture(t, kinds)
			c := loginPanelUser(t, h, "admin")
			for _, path := range []string{"/projects/audit?project=demo&bucket=history", "/admin/bug-reports?bucket=history"} {
				w := c.call("GET", path, nil)
				body := w.Body.String()
				if w.Code != 200 || !strings.Contains(body, tc.notice) || strings.Contains(body, "worker-current") || strings.Contains(body, "보고 내용 current") {
					t.Fatalf("%s: history isolation/notice status=%d", path, w.Code)
				}
				for _, kind := range tc.kinds {
					label := "이전 세대"
					if kind == "unknown" {
						label = "이전 기록(세대 불명)"
					}
					if !strings.Contains(body, label) {
						t.Fatal("row label missing")
					}
				}
				for _, link := range []string{`href="/projects/settings?project=demo"`, `href="/projects/sessions?project=demo"`, `action="/admin/bug-reports/status`} {
					if strings.Contains(body, link) {
						t.Fatal("history links to current project or mutation")
					}
				}
			}
			for _, path := range []string{"/projects/audit?project=demo", "/admin/bug-reports"} {
				w := c.call("GET", path, nil)
				body := w.Body.String()
				if w.Code != 200 || strings.Contains(body, "worker-unknown") || strings.Contains(body, "worker-previous") || strings.Contains(body, "보고 내용 unknown") || strings.Contains(body, "보고 내용 previous") {
					t.Fatal("default is not current-only")
				}
			}
		})
	}
}

func TestGenerationHistoryDetailReadOnlyAndAccess(t *testing.T) {
	s, reports, h := generationHistoryFixture(t, []string{"current", "unknown", "previous"})
	admin := loginPanelUser(t, h, "admin")
	for _, tc := range []struct{ id, bucket, notice string }{{"bug-unknown-001", "unknown", historyUnknownNotice}, {"bug-previous-002", "previous", historyPreviousNotice}} {
		path := "/admin/bug-reports/detail?id=" + tc.id + "&bucket=" + tc.bucket + "&status=all&page=2&severity=critical"
		w := admin.call("GET", path, nil)
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, tc.notice) || strings.Contains(body, `action="/admin/bug-reports/status`) || !strings.Contains(body, "bucket="+tc.bucket+"&amp;page=2&amp;severity=critical&amp;status=all") {
			t.Fatalf("history detail/back link incorrect: status=%d", w.Code)
		}
		if admin.call("GET", "/admin/bug-reports/detail?id="+tc.id, nil).Code != 404 {
			t.Fatal("history detail visible in current")
		}
		if admin.call("GET", "/admin/bug-reports/detail?id=bug-current-000&bucket=history", nil).Code != 404 {
			t.Fatal("current detail visible in history")
		}
		for _, suffix := range []string{"", "?bucket=history"} {
			want := 404
			if suffix != "" {
				want = 403
			}
			if admin.call("POST", "/admin/bug-reports/status"+suffix, url.Values{"id": {tc.id}, "status": {"resolved"}, "expectedVersion": {"fixture-version"}}).Code != want {
				t.Fatal("historical status mutation accepted")
			}
		}
		r, err := reports.GetFor("admin", tc.id, tc.bucket)
		if err != nil || r.Status != "open" {
			t.Fatal("history was changed")
		}
		for _, actor := range []string{"owner", "developer", "viewer", "outsider"} {
			c := loginPanelUser(t, h, actor)
			for _, route := range []string{path, "/admin/bug-reports?bucket=" + tc.bucket, "/projects/audit?project=demo&bucket=" + tc.bucket, "/projects/audit?bucket=history"} {
				if c.call("GET", route, nil).Code != 403 {
					t.Fatalf("%s reached history", actor)
				}
			}
		}
	}
	// Current IDs cannot be mutated through a historical URL either.
	if admin.call("POST", "/admin/bug-reports/status?bucket=history", url.Values{"id": {"bug-current-000"}, "status": {"resolved"}}).Code != 403 {
		t.Fatal("history POST mutated a current report")
	}
	// A permission change takes effect even for the old login session.
	s.mu.Lock()
	u := s.data.Users["admin"]
	u.Role = "user"
	s.data.Users["admin"] = u
	s.mu.Unlock()
	if admin.call("GET", "/admin/bug-reports?bucket=history", nil).Code != 403 || admin.call("GET", "/projects/audit?project=demo&bucket=history", nil).Code != 403 {
		t.Fatal("demoted admin retained history")
	}
}

func TestGenerationHistoryQueriesPagingAndAuditJoin(t *testing.T) {
	kinds := []string{"current"}
	for i := 0; i < 51; i++ {
		kinds = append(kinds, "unknown")
	}
	s, _, h := generationHistoryFixture(t, kinds)
	c := loginPanelUser(t, h, "admin")
	for _, query := range []string{"bucket=", "bucket=bad", "bucket=history&bucket=current"} {
		for _, route := range []string{"/admin/bug-reports?", "/projects/audit?project=demo&"} {
			if c.call("GET", route+query, nil).Code != 400 {
				t.Fatal("invalid bucket accepted")
			}
		}
	}
	w := c.call("GET", "/admin/bug-reports?bucket=history&status=all&severity=critical&page=99", nil)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "총 51건") || !strings.Contains(body, "2 / 2") || !strings.Contains(body, "bucket=history&amp;page=1&amp;severity=critical&amp;status=all") {
		t.Fatal("history count/clamp/filter links failed")
	}
	empty := c.call("GET", "/admin/bug-reports?bucket=history&severity=low", nil).Body.String()
	if !strings.Contains(empty, `href="/admin/bug-reports?bucket=history&amp;page=1&amp;status=open">필터 초기화`) || strings.Contains(empty, historyUnknownNotice) {
		t.Fatal("empty history reset/notice failed")
	}
	if c.call("GET", "/admin/bug-reports?bucket=previous", nil).Code != 200 {
		t.Fatal("previous bucket failed")
	}
	// A restored event shares references with today's tokens. Never resolve these
	// references against the new project generation.
	s.mu.Lock()
	p := s.data.Projects["demo"]
	p.Tokens = append(p.Tokens, SessionToken{ID: "collision-parent", Kind: tokenProject, Label: "CURRENT-PRIVATE-LABEL"})
	s.data.Projects["demo"] = p
	s.audits = map[string]AuditEvent{"old": {Project: "demo", Kind: "registration", At: time.Now().UTC(), ParentID: "collision-parent"}}
	s.data.ScopeIssuanceReady = false
	s.mu.Unlock()
	body = c.call("GET", "/projects/audit?project=demo&bucket=history", nil).Body.String()
	if !strings.Contains(body, "이전 기록(세대 불명)") || strings.Contains(body, "CURRENT-PRIVATE-LABEL") {
		t.Fatal("old audit joined current tokens or blocked by issuance gate")
	}
	s.mu.Lock()
	delete(s.data.Projects, "demo")
	s.mu.Unlock()
	for _, path := range []string{"/projects/audit?project=demo&bucket=history", "/projects/audit?bucket=history"} {
		if c.call("GET", path, nil).Code != 200 {
			t.Fatal("deleted project history inaccessible")
		}
	}
}
