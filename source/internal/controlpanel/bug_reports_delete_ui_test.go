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
	if len(cards) != 6 || cards[len(cards)-1][1] != "리포트 삭제" {
		t.Fatal("delete card must follow all detail cards")
	}
	form := regexp.MustCompile(`(?s)<form[^>]*action="/admin/bug-reports/delete[^>]*>.*?</form>`).FindString(body)
	if !strings.Contains(form, `class="danger"`) || !strings.Contains(form, `class="check"`) || !strings.Contains(form, `value="delete" required`) || strings.Contains(form, " checked") || strings.Contains(form, "data-confirm-action") {
		t.Fatal("required unchecked checkbox and danger button missing")
	}
	if !strings.Contains(body, "본문·client 진단·상태 기록을 영구 삭제합니다. 되돌릴 수 없습니다. 삭제 기록만 남습니다.") {
		t.Fatal("approved description missing")
	}
	stale := deleteForm(r)
	stale.Set("expectedVersion", "stale")
	w = c.call("POST", "/admin/bug-reports/delete", stale)
	body = w.Body.String()
	if w.Code != 409 || !strings.Contains(body, `role="alert" aria-live="assertive"`) || strings.Contains(body, "다시 불러오기") {
		t.Fatal("delete conflict placement/controls")
	}
	if strings.Index(body, "다른 곳에서 리포트가 바뀌었습니다") > strings.Index(body, "처리 상태</h2>") {
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
	for _, want := range []string{"<h1>버그 리포트</h1>", r.ID, "← 목록", `class="notice notice-info"`, `role="status"`, `관리자(admin)가 삭제했습니다.`} {
		if !strings.Contains(body, want) {
			t.Fatal("absent page missing", want)
		}
	}
	if !strings.Contains(body, "page=2&amp;status=all") {
		t.Fatal("filtered list link missing")
	}
	if strings.Contains(body, "삭제합니다</label>") {
		t.Fatal("absent has delete form")
	}
}
func TestBugReportDeleteSuccessNoticeIncludesExactID(t *testing.T) {
	_, _, c, _, r := deletePanelFixture(t)
	w := c.call("POST", "/admin/bug-reports/delete", url.Values{"id": {r.ID}, "expectedVersion": {r.ExpectedVersion}, "confirm": {"delete"}})
	req := noticeRedirectRequest(t, c, w)
	body := req.Body.String()
	if !strings.Contains(body, "리포트 "+r.ID+"를 삭제했습니다.") {
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
