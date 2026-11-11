package controlpanel

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestBugReportResolvedDetailShowsActionsAndHistory(t *testing.T) {
	r := bugReportFixture()
	r.Status = "closed"
	r.Resolution = "resolved"
	r.Note = "MR fixed"
	r.StatusChanges = []bugreport.StatusChange{{At: r.ReceivedAt, From: "open", To: "resolved", Actor: "admin", Source: "mcp", Session: &bugreport.ChangeSession{Name: "worker", RegistrationID: "fixture-reg"}, Note: "MR fixed"}}
	c := loginPanelUser(t, bugReportTestHandler(t, &fakeBugReports{reports: []bugreport.Report{r}}), "admin")
	w := c.call("GET", "/admin/bug-reports/detail?id="+r.ID, nil)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`value="open">` + en("bug.action.reopen") + `</button>`, `value="ignored">` + en("bug.action.ignore_change") + `</button>`, en("bug.handling"), en("bug.history"), en("bug.agent"), en("bug.state.open") + " → " + en("bug.state.resolved"), "MR fixed"} {
		if !strings.Contains(body, want) {
			t.Errorf("resolved detail missing %q", want)
		}
	}
}

func TestBugReportStatusFiltersBadgesAndNoteExcerpt(t *testing.T) {
	rows := []bugreport.Report{}
	for _, state := range []string{"open", "resolved", "ignored", "closed"} {
		r := bugReportFixture()
		r.ID = "bug-" + state
		r.Note = "note <tag>\nsecond line"
		r.Status = state
		if state == "resolved" || state == "ignored" {
			r.Status = "closed"
			r.Resolution = state
		}
		rows = append(rows, r)
	}
	c := loginPanelUser(t, bugReportTestHandler(t, &fakeBugReports{reports: rows}), "admin")
	for _, state := range []string{"open", "resolved", "ignored", "closed", "all"} {
		w := c.call("GET", "/admin/bug-reports?status="+state, nil)
		if w.Code != 200 {
			t.Fatal(state, w.Code)
		}
		body := w.Body.String()
		for _, r := range rows {
			present := strings.Contains(body, `id=`+r.ID+`&amp;`)
			if present != (state == "all" || state == r.State()) {
				t.Fatal("state filter", state, r.State())
			}
		}
		if !strings.Contains(body, "bug-note-excerpt") || strings.Contains(body, "note <tag>") {
			t.Fatal("note excerpt/escape")
		}
		if state != "all" && !strings.Contains(body, reportStateClass(state)) {
			t.Fatal("badge class", state)
		}
	}
}
func TestBugReportStatusConflictRendersLatestAndKeepsNote(t *testing.T) {
	s, b, h := misuseFixture(t)
	r := bugReportFixture()
	r.Reporter.ProjectGenerationID = s.ProjectGeneration("demo")
	saved, err := b.reports.Create(r.Input, r.Reporter, "test")
	if err != nil {
		t.Fatal(err)
	}
	stale := saved.ExpectedVersion
	saved, _, err = b.reports.SetResolution(saved.ID, "resolved", "saved note", bugreport.Actor{UserID: "admin", Source: "panel"}, stale)
	if err != nil {
		t.Fatal(err)
	}
	c := loginPanelUser(t, h, "admin")
	note := "typed <script> note\nkept"
	w := c.call("POST", "/admin/bug-reports/status?status=open&severity=critical&page=2", url.Values{"id": {saved.ID}, "status": {"ignored"}, "expectedVersion": {stale}, "note": {note}})
	body := w.Body.String()
	for _, want := range []string{enHTML("bug.status_conflict"), en("bug.reload"), "aria-live=\"assertive\"", "typed &lt;script&gt; note", saved.ExpectedVersion, "page=2", en("bug.handling"), en("bug.history")} {
		if !strings.Contains(body, want) {
			t.Errorf("conflict missing %q", want)
		}
	}
	if strings.Contains(body, "notice-info") {
		t.Error("conflict showed success notice")
	}
	if w.Code != 409 || !strings.Contains(w.Header().Get("Content-Type"), "text/html") || strings.Contains(body, "typed <script>") {
		t.Fatal("not a safe rendered conflict", w.Code)
	}
	got, _ := b.reports.Get(saved.ID)
	if got.ExpectedVersion != saved.ExpectedVersion || got.Note != "saved note" {
		t.Fatal("conflict wrote")
	}
	w = c.call("POST", "/admin/bug-reports/status?status=open&page=2", url.Values{"id": {saved.ID}, "status": {"ignored"}, "expectedVersion": {saved.ExpectedVersion}, "note": {"classified"}})
	if w.Code != 303 {
		t.Fatal("classification", w.Code)
	}
	body = c.call("GET", w.Header().Get("Location"), nil).Body.String()
	if strings.Contains(body, "notice-info") || !strings.Contains(body, `value="resolved">`+en("bug.action.resolve_change")+`</button>`) {
		t.Fatal("success out-of-filter state")
	}
}
func TestBugReportLegacyReclassifyAndHistoryReadonly(t *testing.T) {
	_, reports, h := generationHistoryFixture(t, []string{"current", "previous"})
	r, err := reports.Get("bug-current-000")
	if err != nil {
		t.Fatal(err)
	}
	if err = reports.SetStatus(r.ID, "closed", "admin"); err != nil {
		t.Fatal(err)
	}
	r, _ = reports.Get(r.ID)
	c := loginPanelUser(t, h, "admin")
	body := c.call("GET", "/admin/bug-reports/detail?id="+r.ID, nil).Body.String()
	for _, want := range []string{enHTML("bug.state.closed"), "value=\"resolved\"", "value=\"ignored\"", "value=\"open\""} {
		if !strings.Contains(body, want) {
			t.Fatal("legacy action", want)
		}
	}
	w := c.call("POST", "/admin/bug-reports/status", url.Values{"id": {r.ID}, "status": {"resolved"}, "expectedVersion": {r.ExpectedVersion}, "note": {"reclassified"}})
	if w.Code != 303 {
		t.Fatal(w.Code)
	}
	r, _ = reports.Get(r.ID)
	if r.Resolution != "resolved" {
		t.Fatal("reclassify")
	}
	body = c.call("GET", "/admin/bug-reports/detail?id=bug-previous-001&bucket=history", nil).Body.String()
	if strings.Contains(body, "bug-status-form") || !strings.Contains(body, en("bug.read_only")) || !strings.Contains(body, en("bug.history")) {
		t.Fatal("history mutable")
	}
}

func TestBugReportNormalDetailStartsWithEmptyNote(t *testing.T) {
	for _, state := range []string{"open", "resolved", "ignored", "closed"} {
		t.Run(state, func(t *testing.T) {
			r := bugReportFixture()
			r.Note = "stored note"
			r.Status = state
			if state == "resolved" || state == "ignored" {
				r.Status = "closed"
				r.Resolution = state
			}
			c := loginPanelUser(t, bugReportTestHandler(t, &fakeBugReports{reports: []bugreport.Report{r}}), "admin")
			body := c.call("GET", "/admin/bug-reports/detail?id="+r.ID, nil).Body.String()
			note := regexp.MustCompile(`(?s)<textarea[^>]*name="note"[^>]*>(.*?)</textarea>`).FindStringSubmatch(body)
			if len(note) != 2 || note[1] != "" {
				t.Error("normal render prefilled stored note")
			}
			if strings.Contains(body, "notice-info") {
				t.Error("plain render showed success notice")
			}
		})
	}
}
func TestBugReportChangeNoticeIsSingleUseAndFilterAware(t *testing.T) {
	h := bugReportTestHandler(t, &fakeBugReports{reports: []bugreport.Report{bugReportFixture()}})
	c := loginPanelUser(t, h, "admin")
	for _, tc := range []struct{ state, message string }{{"resolved", enHTML("notice.bug-report-resolved")}, {"ignored", enHTML("notice.bug-report-ignored")}, {"open", enHTML("notice.bug-report-open")}} {
		post := c.call("POST", "/admin/bug-reports/status?status=open&page=2", url.Values{"id": {"bug-demo-004"}, "status": {tc.state}, "note": {"new note"}, "expectedVersion": {"fixture-version"}})
		if post.Code != 303 {
			t.Fatal(post.Code)
		}
		get := httptest.NewRequest(http.MethodGet, "http://broker"+post.Header().Get("Location"), nil)
		get.AddCookie(c.cookie)
		for _, cookie := range post.Result().Cookies() {
			get.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		h.ServeHTTP(response, get)
		body := response.Body.String()
		want := tc.message
		if tc.state != "open" {
			want += " " + enHTML("notice.not_in_current_filter")
		}
		if !strings.Contains(body, want) || !strings.Contains(body, `class="notice notice-info"`) {
			t.Errorf("success notice missing %q", want)
		}
		replay := httptest.NewRecorder()
		h.ServeHTTP(replay, get)
		if strings.Contains(replay.Body.String(), "notice-info") {
			t.Error("success notice replayed")
		}
	}
}
