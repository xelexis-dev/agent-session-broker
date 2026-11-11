package controlpanel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

type announcementBackend struct {
	panelBackend
	calls                int
	actor, project, body string
}

func (b *announcementBackend) Announce(_ context.Context, actor, project, body string) ([]AnnouncementResult, error) {
	b.calls++
	b.actor, b.project, b.body = actor, project, body
	return []AnnouncementResult{{Project: "demo", Name: "ready", Accepted: true}, {Project: "demo", Name: "full", Error: "queue-full"}}, nil
}
func TestAnnouncementHTTPAdminAndValidation(t *testing.T) {
	b := &announcementBackend{}
	h := New(userStore(t), b, http.NotFoundHandler())
	a := loginPanelUser(t, h, "admin")
	u := loginPanelUser(t, h, "owner")
	for _, who := range []string{"owner", "developer", "viewer"} {
		c := loginPanelUser(t, h, who)
		if w := c.call("POST", "/admin/announcement", url.Values{"body": {"hello"}}); w.Code != 403 {
			t.Errorf("%s status %d", who, w.Code)
		}
	}
	if body := u.call("GET", "/profile", nil).Body.String(); strings.Contains(body, `action="/admin/announcement"`) {
		t.Error("nonadmin UI exposed")
	}
	req := httptest.NewRequest("POST", "http://broker/admin/announcement", strings.NewReader("body=notice"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(a.cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Errorf("missing CSRF =%d", w.Code)
	}
	page := a.call("GET", "/admin/settings", nil)
	if !strings.Contains(page.Body.String(), `action="/admin/announcement"`) {
		t.Error("admin composer missing")
	}
	for _, body := range []string{"", "  ", strings.Repeat("x", 12000)} {
		if w := a.call("POST", "/admin/announcement", url.Values{"body": {body}}); w.Code != 400 {
			t.Errorf("invalid len%d =%d", len(body), w.Code)
		}
	}
	if w := a.call("POST", "/admin/announcement", url.Values{"body": {"notice"}, "project": {"unknown"}}); w.Code != 400 {
		t.Errorf("unknown project %d", w.Code)
	}
	for _, project := range []string{"", "demo"} {
		w := a.call("POST", "/admin/announcement", announcementForm(a, "notice", project))
		if w.Code != 303 || b.project != project || b.actor != "admin" || b.body != "notice" {
			t.Errorf("scope %q = %d %+v", project, w.Code, b)
		}
		w = a.call("GET", w.Header().Get("Location"), nil)
		for _, text := range []string{"ready", "full", enHTML("admin.announce.error_queue_full"), enHTML("admin.announce.counts", "accepted", 1, "failed", 1)} {
			if !strings.Contains(w.Body.String(), text) {
				t.Errorf("result lacks %s", text)
			}
		}
	}
}
func TestAnnouncementReservedApprovalNickname(t *testing.T) {
	s := userStore(t)
	raw, e := s.IssueProjectTokenFor("owner", "demo", "notice test")
	if e != nil {
		t.Fatal(e)
	}
	code, e := s.BeginDeviceAuthorization("demo", raw, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "notice-session", InstanceID: "pc-example", RequestID: "notice-request"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ApproveDeviceAuthorization("owner", code.UserCode, "broker-admin", false); e == nil {
		t.Fatal("reserved approval nickname accepted")
	}
}

func TestAnnouncementUnicodeBodyFitsEnvelopeDespiteFormEncoding(t *testing.T) {
	b := &announcementBackend{}
	h := New(userStore(t), b, http.NotFoundHandler())
	a := loginPanelUser(t, h, "admin")
	body := strings.Repeat("공지", 1000)
	if w := a.call("POST", "/admin/announcement", announcementForm(a, body, "")); w.Code != 303 || b.body != body {
		t.Fatalf("6000 UTF8 bytes within envelope rejected by form transport: %d", w.Code)
	}
}

// The HTTP/backend boundary is where a duplicate would enqueue a second batch.
func announcementForm(a *userPanelClient, body, project string) url.Values {
	page := a.call("GET", "/admin/settings", nil)
	match := regexp.MustCompile(`name="announcement_token" value="([^"]+)"`).FindStringSubmatch(page.Body.String())
	values := url.Values{"body": {body}, "project": {project}}
	// Before the feature exists, exercise the original form rather than a new unknown key.
	if len(match) == 2 {
		values.Set("announcement_token", match[1])
	}
	return values
}
func TestAnnouncementDuplicatePOSTDoesNotEnqueue(t *testing.T) {
	b := &announcementBackend{}
	h := New(userStore(t), b, http.NotFoundHandler())
	a := loginPanelUser(t, h, "admin")
	form := announcementForm(a, "notice", "")
	first := a.call("POST", "/admin/announcement", form)
	second := a.call("POST", "/admin/announcement", form)
	if b.calls != 1 {
		t.Errorf("same form enqueued %d batches, want 1", b.calls)
	}
	if first.Code != 303 || second.Code != 303 {
		t.Errorf("POST status = %d/%d, want 303/303", first.Code, second.Code)
	}
	if second.Header().Get("Location") != "" {
		result := a.call("GET", second.Header().Get("Location"), nil)
		if result.Code != 200 || !strings.Contains(result.Body.String(), enHTML("admin.announce.duplicate")) {
			t.Error("duplicate result lacks already accepted notice")
		}
	}
}
func TestAnnouncementPOSTRedirectAndRefreshKeepsCounts(t *testing.T) {
	b := &announcementBackend{}
	h := New(userStore(t), b, http.NotFoundHandler())
	a := loginPanelUser(t, h, "admin")
	post := a.call("POST", "/admin/announcement", announcementForm(a, "notice", "demo"))
	if post.Code != 303 {
		t.Fatalf("POST = %d, want 303", post.Code)
	}
	for i := 0; i < 2; i++ {
		result := a.call("GET", post.Header().Get("Location"), nil)
		if result.Code != 200 || !strings.Contains(result.Body.String(), enHTML("admin.announce.counts", "accepted", 1, "failed", 1)) {
			t.Error("GET result lacks aggregate")
		}
		if policy := result.Header().Get("Referrer-Policy"); policy != "same-origin" {
			t.Errorf("result page contains POST forms: Referrer-Policy = %q, want same-origin", policy)
		}
	}
	if b.calls != 1 {
		t.Errorf("refresh enqueued %d batches", b.calls)
	}
	other := loginPanelUser(t, h, "admin")
	if got := other.call("GET", post.Header().Get("Location"), nil); got.Code != 403 {
		t.Errorf("other login read result: %d", got.Code)
	}
}
func TestAnnouncementMissingTokenRejected(t *testing.T) {
	b := &announcementBackend{}
	h := New(userStore(t), b, http.NotFoundHandler())
	a := loginPanelUser(t, h, "admin")
	w := a.call("POST", "/admin/announcement", url.Values{"body": {"notice"}})
	if w.Code != 403 || b.calls != 0 {
		t.Errorf("missing token status=%d enqueues=%d", w.Code, b.calls)
	}
}
func TestAnnouncementExpiredTokenRejected(t *testing.T) {
	b := &announcementBackend{}
	p := &panel{store: userStore(t), backend: b, next: http.NotFoundHandler(), sessions: map[string]loginSession{}}
	h := http.HandlerFunc(p.serve)
	a := loginPanelUser(t, h, "admin")
	form := announcementForm(a, "notice", "")
	p.mu.Lock()
	if record := p.announcements[form.Get("announcement_token")]; record != nil {
		record.expires = time.Now().Add(-time.Second)
	}
	p.mu.Unlock()
	w := a.call("POST", "/admin/announcement", form)
	if w.Code != 403 || b.calls != 0 {
		t.Errorf("expired token status=%d enqueues=%d", w.Code, b.calls)
	}
}
func TestAnnouncementTokenCannotBeUsedByAnotherLogin(t *testing.T) {
	b := &announcementBackend{}
	h := New(userStore(t), b, http.NotFoundHandler())
	a := loginPanelUser(t, h, "admin")
	other := loginPanelUser(t, h, "admin")
	w := other.call("POST", "/admin/announcement", announcementForm(a, "notice", ""))
	if w.Code != 403 || b.calls != 0 {
		t.Errorf("other login used token: status=%d enqueues=%d", w.Code, b.calls)
	}
}
func TestAnnouncementMixedCaseApprovalNicknameRejected(t *testing.T) {
	for _, name := range []string{"Broker-Admin", "BROKER-ADMIN"} {
		t.Run(name, func(t *testing.T) {
			s := userStore(t)
			raw, err := s.IssueProjectTokenFor("owner", "demo", "notice test")
			if err != nil {
				t.Fatal(err)
			}
			code, err := s.BeginDeviceAuthorization("demo", raw, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "notice-session", InstanceID: "pc-example", RequestID: "notice-request"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ApproveDeviceAuthorization("owner", code.UserCode, name, false); err == nil {
				t.Error("mixed-case reserved approval nickname accepted")
			}
		})
	}
}
