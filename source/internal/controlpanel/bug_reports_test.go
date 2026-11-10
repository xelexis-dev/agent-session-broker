package controlpanel

import (
	"errors"
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeBugReports struct {
	mu      sync.Mutex
	reports []bugreport.Report
	err     error
	calls   int
	filters []bugreport.Filter
	pages   []int
}

func (s *fakeBugReports) List(f bugreport.Filter, page int) ([]bugreport.Report, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.filters = append(s.filters, f)
	s.pages = append(s.pages, page)
	if s.err != nil {
		return nil, 0, s.err
	}
	var rows []bugreport.Report
	for _, r := range s.reports {
		if (f.Status == "" || f.Status == r.Status) && (f.State == "" || f.State == "all" || f.State == r.State()) && (f.Severity == "" || f.Severity == r.Severity) {
			rows = append(rows, r)
		}
	}
	start := min((page-1)*50, len(rows))
	return rows[start:min(start+50, len(rows))], len(rows), nil
}
func (s *fakeBugReports) Get(id string) (bugreport.Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return bugreport.Report{}, s.err
	}
	for _, r := range s.reports {
		if r.ID == id {
			return r, nil
		}
	}
	return bugreport.Report{}, bugreport.ErrNotFound
}
func (s *fakeBugReports) SetResolution(id, state, note string, actor bugreport.Actor, version string) (bugreport.Report, bool, error) {
	if state != "resolved" && state != "ignored" && state != "open" || version == "" {
		return bugreport.Report{}, false, bugreport.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return bugreport.Report{}, false, s.err
	}
	for i := range s.reports {
		if s.reports[i].ID == id {
			r := &s.reports[i]
			from := r.State()
			r.Status = state
			r.Resolution = ""
			if state != "open" {
				r.Status = "closed"
				r.Resolution = state
			}
			r.Note = note
			r.StatusChangedAt = time.Now().UTC()
			r.StatusChangedBy = actor.UserID
			r.StatusChanges = append(r.StatusChanges, bugreport.StatusChange{At: r.StatusChangedAt, From: from, To: state, Actor: actor.UserID, Source: actor.Source, Note: note})
			return *r, true, nil
		}
	}
	return bugreport.Report{}, false, bugreport.ErrNotFound
}
func bugReportFixture() bugreport.Report {
	at := time.Date(2026, 10, 21, 2, 34, 0, 0, time.UTC)
	return bugreport.Report{ID: "bug-demo-004", ExpectedVersion: "fixture-version", Status: "open", ReceivedAt: at, ServerVersion: "0.17.1", Reporter: bugreport.Reporter{ProjectGenerationID: "AAAAAAAAAAAAAAAAAAAAAAAAAA", Project: "demo", SessionName: "worker-04", RegistrationID: "REG-DEMO-004", Harness: "Codex", OwnerID: "owner"}, Input: bugreport.Input{Title: "수신 준비 상태가 갱신되지 않음", Summary: "메시지가 도착했지만 수신 준비 상태가 갱신되지 않습니다.", Steps: "1. 세션을 연결하고 수신을 시작합니다.\n2. 다른 세션에서 메시지를 보낸 뒤 상태를 확인합니다.", Expected: "메시지를 받을 수 있으면 수신 준비 상태가 표시됩니다.", Actual: "메시지는 수신되지만 준비 안 됨으로 남아 있습니다.", Tool: "auth_status · send_message", Severity: "critical", Client: &bugreport.ClientDiagnostics{ClientVersion: "0.17.1", ServerVersion: "0.17.0", Harness: "codex", OS: "macOS", Arch: "arm64", AuthState: "authorized", Events: []bugreport.DiagnosticEvent{{At: at.Add(-4 * time.Second), Event: "receive-failed", ReasonClass: "network"}, {At: at.Add(-6 * time.Second), Event: "reconnected"}, {At: at.Add(-8 * time.Second), Event: "handover-skipped", ReasonClass: "unsupported-mode"}}}}}
}
func bugReportTestHandler(t *testing.T, store *fakeBugReports) http.Handler {
	t.Helper()
	p := &panel{store: userStore(t), backend: &panelBackend{}, next: http.NotFoundHandler(), sessions: map[string]loginSession{}, bugReports: store, version: "0.17.1"}
	return http.NewCrossOriginProtection().Handler(http.HandlerFunc(p.serve))
}
func TestBugReportAdminList(t *testing.T) {
	store := &fakeBugReports{reports: []bugreport.Report{bugReportFixture()}}
	c := loginPanelUser(t, bugReportTestHandler(t, store), "admin")
	for path, wants := range map[string][]string{
		"/admin/bug-reports":                        {"버그 리포트", "worker-04", "demo", "0.17.1", "치명적", "1 / 1"},
		"/admin/bug-reports/detail?id=bug-demo-004": {"worker-04", "demo", "REG-DEMO-004", "client 진단", "0.17.0", "접수 서버 버전"},
	} {
		w := c.call("GET", path, nil)
		if w.Code != 200 {
			t.Fatalf("%s status=%d", path, w.Code)
		}
		for _, want := range wants {
			if !strings.Contains(w.Body.String(), want) {
				t.Errorf("%s missing %q", path, want)
			}
		}
		if !strings.Contains(w.Body.String(), "</html>") {
			t.Fatal("template did not finish")
		}
	}
}
func TestBugReportAccessAndCSRF(t *testing.T) {
	store := &fakeBugReports{reports: []bugreport.Report{bugReportFixture()}}
	h := bugReportTestHandler(t, store)
	for _, user := range []string{"owner", "developer", "viewer", "outsider"} {
		c := loginPanelUser(t, h, user)
		for _, path := range []string{"/admin/bug-reports", "/admin/bug-reports/detail?id=bug-demo-004", "/admin/bug-reports/status"} {
			method := "GET"
			if strings.HasSuffix(path, "status") {
				method = "POST"
			}
			if w := c.call(method, path, url.Values{"id": {"bug-demo-004"}, "status": {"resolved"}}); w.Code != 403 {
				t.Errorf("%s %s status=%d", user, path, w.Code)
			}
		}
		if strings.Contains(c.call("GET", "/", nil).Body.String(), `href="/admin/bug-reports"`) {
			t.Fatal("admin nav exposed")
		}
	}
	if store.calls != 0 {
		t.Fatal("unauthorized request reached store")
	}
	c := loginPanelUser(t, h, "admin")
	c.csrf = "invalid"
	if w := c.call("POST", "/admin/bug-reports/status", url.Values{"id": {"bug-demo-004"}, "status": {"resolved"}}); w.Code != 403 {
		t.Fatal("CSRF accepted")
	}
	if store.calls != 0 {
		t.Fatal("invalid CSRF changed store")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/admin/bug-reports", nil))
	if w.Code != 303 {
		t.Fatal("anonymous access exposed")
	}
}
func TestBugReportFiltersPagesAndEmpty(t *testing.T) {
	store := &fakeBugReports{}
	for i := 0; i < 51; i++ {
		r := bugReportFixture()
		r.ID = fmt.Sprintf("bug-%03d", i)
		r.Status = "closed"
		store.reports = append(store.reports, r)
	}
	c := loginPanelUser(t, bugReportTestHandler(t, store), "admin")
	w := c.call("GET", "/admin/bug-reports?status=closed&severity=critical&page=9999", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "2 / 2") || len(store.pages) != 2 || store.pages[1] != 2 || store.filters[1].State != "closed" || store.filters[1].Severity != "critical" {
		t.Fatal("filtered page not clamped")
	}
	for _, want := range []string{"bug-050", "page=1&amp;severity=critical&amp;status=closed", "page=2&amp;severity=critical&amp;status=closed"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, q := range []string{"status=bad", "severity=bad", "page=abc", "page=9999999999999999999999999", "status=open&status=closed", "page=1&page=2"} {
		if w := c.call("GET", "/admin/bug-reports?"+q, nil); w.Code != 400 {
			t.Errorf("invalid filter accepted %q", q)
		}
	}
	for query, want := range map[string]string{"": "받은 버그 리포트가 없습니다.", "?status=open&severity=low": "조건에 맞는 버그 리포트가 없습니다."} {
		w := c.call("GET", "/admin/bug-reports"+query, nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) || strings.Contains(w.Body.String(), `class="bug-pagination"`) {
			t.Fatal("empty state/pagination incorrect")
		}
		if query != "" && !strings.Contains(w.Body.String(), "필터 초기화") {
			t.Fatal("filter reset missing")
		}
	}
	w = c.call("GET", "/admin/bug-reports?status=all&page=-2", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "1 / 2") || store.filters[len(store.filters)-1].State != "all" {
		t.Fatal("all status/page minimum failed")
	}
}
func TestBugReportStatusAndErrors(t *testing.T) {
	store := &fakeBugReports{reports: []bugreport.Report{bugReportFixture()}}
	c := loginPanelUser(t, bugReportTestHandler(t, store), "admin")
	for _, status := range []string{"resolved", "open"} {
		w := c.call("POST", "/admin/bug-reports/status?status=all&severity=critical&page=2", url.Values{"id": {"bug-demo-004"}, "status": {status}, "expectedVersion": {"fixture-version"}})
		if w.Code != 303 || w.Header().Get("Location") != "/admin/bug-reports/detail?id=bug-demo-004&page=2&severity=critical&status=all" {
			t.Fatal("status PRG/filter preservation failed")
		}
		r := store.reports[0]
		if r.State() != status || r.StatusChangedBy != "admin" || r.StatusChangedAt.IsZero() {
			t.Fatal("actor/status not recorded")
		}
		body := c.call("GET", w.Header().Get("Location"), nil).Body.String()
		want := "admin · 열림 → 해결 ·"
		button := "다시 열기"
		if status == "open" {
			want = "admin · 해결 → 열림 ·"
			button = "해결"
		}
		if !strings.Contains(body, want) || !strings.Contains(body, ">"+button+"</button>") {
			t.Fatal("latest status line/button incorrect")
		}
	}
	before := store.calls
	for _, values := range []url.Values{{"id": {"bug-demo-004"}, "status": {"invalid"}}, {"id": {"bug-demo-004"}, "status": {"resolved"}, "actorUserID": {"outsider"}}, {"id": {"bug-demo-004"}, "status": {"open", "closed"}}} {
		if c.call("POST", "/admin/bug-reports/status", values).Code != 400 {
			t.Fatal("invalid status form accepted")
		}
	}
	if store.calls != before {
		t.Fatal("invalid form reached store")
	}
	if c.call("GET", "/admin/bug-reports/status", nil).Code != 405 || c.call("POST", "/admin/bug-reports", nil).Code != 405 {
		t.Fatal("unsafe methods accepted")
	}
	if c.call("GET", "/admin/bug-reports/detail?id=missing", nil).Code != 404 {
		t.Fatal("unknown ID not 404")
	}
	if c.call("POST", "/admin/bug-reports/status", url.Values{"id": {"missing"}, "status": {"open"}, "expectedVersion": {"fixture-version"}}).Code != 404 {
		t.Fatal("unknown status target not 404")
	}
	store.err = errors.New("private internal error")
	for _, path := range []string{"/admin/bug-reports", "/admin/bug-reports/detail?id=bug-demo-004"} {
		w := c.call("GET", path, nil)
		if w.Code != 500 || strings.Contains(w.Body.String(), "private internal") {
			t.Fatal("store error leaked")
		}
	}
}
func TestBugReportDiagnosticsAndEscaping(t *testing.T) {
	r := bugReportFixture()
	r.Title = `<script>alert(1)</script>`
	r.Summary = "first\nsecond <b>raw</b>"
	r.Steps = ""
	r.Client = nil
	store := &fakeBugReports{reports: []bugreport.Report{r}}
	c := loginPanelUser(t, bugReportTestHandler(t, store), "admin")
	body := c.call("GET", "/admin/bug-reports/detail?id=bug-demo-004", nil).Body.String()
	for _, want := range []string{"client 진단 없음", "진단 정보가 전송되지 않았습니다.", "&lt;script&gt;", "first\nsecond &lt;b&gt;raw&lt;/b&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "<script>alert") || strings.Contains(body, "<dt>재현 단계</dt>") {
		t.Fatal("unsafe/raw or optional empty field")
	}
	store.reports[0].Client = &bugreport.ClientDiagnostics{LastFollow: "must-not-show-for-codex"}
	body = c.call("GET", "/admin/bug-reports/detail?id=bug-demo-004", nil).Body.String()
	if !strings.Contains(body, "최근 진단 이벤트가 없습니다.") || !strings.Contains(body, "준비 안 됨") || strings.Contains(body, "must-not-show") {
		t.Fatal("partial diagnostics wrong")
	}
	store.reports[0].Reporter.Harness = "claude"
	store.reports[0].Client.LastFollow = "followed: resumed"
	for i := 0; i < 21; i++ {
		store.reports[0].Client.Events = append(store.reports[0].Client.Events, bugreport.DiagnosticEvent{Event: fmt.Sprintf("event-%02d", i)})
	}
	body = c.call("GET", "/admin/bug-reports/detail?id=bug-demo-004", nil).Body.String()
	if !strings.Contains(body, "followed: resumed") || !strings.Contains(body, "event-19") || strings.Contains(body, "event-20") {
		t.Fatal("follow/event limit incorrect")
	}
}

type reportPanelBackend struct {
	panelBackend
	reports *bugreport.Store
}

func (b *reportPanelBackend) BugReports() *bugreport.Store { return b.reports }

func TestBugReportBackendStore(t *testing.T) {
	reports, err := bugreport.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer reports.Close()
	fixture := bugReportFixture()
	saved, err := reports.Create(fixture.Input, fixture.Reporter, fixture.ServerVersion)
	if err != nil {
		t.Fatal(err)
	}
	s := userStore(t)
	reports.SetProjectPolicy(func(actor string) (map[string]string, func(), error) {
		if actor != "admin" && actor != "" {
			return nil, nil, bugreport.ErrForbidden
		}
		return map[string]string{"demo": fixture.Reporter.ProjectGenerationID}, func() {}, nil
	})
	h := New(s, &reportPanelBackend{reports: reports}, http.NotFoundHandler())
	c := loginPanelUser(t, h, "admin")
	w := c.call("GET", "/admin/bug-reports", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), saved.ID) {
		t.Fatal("request store not used")
	}
	w = c.call("POST", "/admin/bug-reports/status", url.Values{"id": {saved.ID}, "status": {"resolved"}, "expectedVersion": {saved.ExpectedVersion}})
	if w.Code != 303 {
		t.Fatal("real store update failed")
	}
	after, err := reports.Get(saved.ID)
	if err != nil || after.Status != "closed" || after.StatusChangedBy != "admin" {
		t.Fatal("real store status not persisted")
	}
	if c.call("POST", "/admin/bug-reports/status", url.Values{"id": {saved.ID}, "status": {"invalid"}}).Code != 400 {
		t.Fatal("store validation not respected")
	}
	plain := loginPanelUser(t, New(s, &panelBackend{}, http.NotFoundHandler()), "admin")
	if plain.call("GET", "/admin/bug-reports", nil).Code != 503 {
		t.Fatal("missing store not unavailable")
	}
}
