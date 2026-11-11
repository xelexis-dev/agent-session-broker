package controlpanel

import (
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestBugReportDeleteVisualStructure(t *testing.T) {
	_, _, c, _, r := deletePanelFixture(t)
	w := c.call("GET", "/admin/bug-reports/detail?id="+r.ID, nil)
	body := w.Body.String()
	cards := regexp.MustCompile(`<section class="card[^>]*>.*?<h2[^>]*>([^<]+)</h2>`).FindAllStringSubmatch(body, -1)
	if len(cards) != 6 || cards[len(cards)-1][1] != en("bug.delete") {
		t.Fatal("delete card must follow all detail cards")
	}
	form := regexp.MustCompile(`(?s)<form[^>]*action="/admin/bug-reports/delete[^>]*>.*?</form>`).FindString(body)
	if !strings.Contains(form, `class="danger"`) || !strings.Contains(form, `class="check"`) || !strings.Contains(form, `value="delete" required`) || strings.Contains(form, " checked") || strings.Contains(form, "data-confirm-action") {
		t.Fatal("required unchecked checkbox and danger button missing")
	}
	if !strings.Contains(body, enHTML("bug.delete_help")) {
		t.Fatal("approved description missing")
	}
	stale := deleteForm(r)
	stale.Set("expectedVersion", "stale")
	w = c.call("POST", "/admin/bug-reports/delete", stale)
	body = w.Body.String()
	if w.Code != 409 || !strings.Contains(body, `role="alert" aria-live="assertive"`) || strings.Contains(body, ">"+en("bug.reload")+"</a>") {
		t.Fatal("delete conflict placement/controls")
	}
	if strings.Index(body, enHTML("bug.delete_conflict")) > strings.Index(body, en("bug.handling")+"</h2>") {
		t.Fatal("notice must precede first card")
	}
}
func TestBugReportDeleteAbsentPage(t *testing.T) {
	_, _, c, _, r := deletePanelFixture(t)
	if w := c.call("POST", "/admin/bug-reports/delete?status=all&page=2", deleteForm(r)); w.Code != 303 {
		t.Fatal(w.Code)
	}
	w := c.call("GET", "/admin/bug-reports/detail?id="+r.ID+"&status=all&page=2", nil)
	body := w.Body.String()
	for _, want := range []string{"<h1>" + en("bug.title_fallback") + "</h1>", r.ID, en("bug.back"), `class="notice notice-info"`, `role="status"`, enHTML("bug.deleted", "actor", "admin")} {
		if !strings.Contains(body, want) {
			t.Fatal("absent page missing", want)
		}
	}
	if !strings.Contains(body, "page=2&amp;status=all") {
		t.Fatal("filtered list link missing")
	}
	if strings.Contains(body, en("bug.delete_confirm")+"</label>") {
		t.Fatal("absent has delete form")
	}
}
func TestBugReportDeleteSuccessNoticeIncludesExactID(t *testing.T) {
	_, _, c, _, r := deletePanelFixture(t)
	w := c.call("POST", "/admin/bug-reports/delete", url.Values{"id": {r.ID}, "expectedVersion": {r.ExpectedVersion}, "confirm": {"delete"}})
	req := noticeRedirectRequest(t, c, w)
	body := req.Body.String()
	if !strings.Contains(body, en("notice.bug-report-deleted-id", "id", r.ID)) {
		t.Fatal("exact success wording missing")
	}
}

func noticeRedirectRequest(t *testing.T, c *userPanelClient, w *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "http://broker"+w.Header().Get("Location"), nil)
	req.AddCookie(c.cookie)
	for _, cookie := range w.Result().Cookies() {
		req.AddCookie(cookie)
	}
	next := httptest.NewRecorder()
	c.h.ServeHTTP(next, req)
	return next
}
