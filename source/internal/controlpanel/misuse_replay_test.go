package controlpanel

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func misuseDevice(t *testing.T, s *Store, project, actor string) DeviceResponse {
	t.Helper()
	raw, err := s.IssueProjectTokenFor(actor, project, "synthetic label")
	if err != nil {
		t.Fatal(err)
	}
	response, err := s.BeginDeviceAuthorization(project, raw, DeviceRequest{ClientID: "asb-client-v1", Harness: "claude", SessionID: "synthetic-conversation", InstanceID: "synthetic-instance", RequestID: "synthetic-request"})
	if err != nil {
		t.Fatal(err)
	}
	return response
}
func TestMisuseC7TokenDuplicateSubmission(t *testing.T) {
	s, _, h := misuseFixture(t)
	c := loginPanelUser(t, h, "owner")
	page := c.call("GET", "/projects/settings?project=demo", nil)
	match := regexp.MustCompile(`name="issuance_token" value="([^"]+)"`).FindStringSubmatch(page.Body.String())
	if len(match) != 2 {
		t.Fatal("issuance form nonce missing")
	}
	form := url.Values{"project": {"demo"}, "label": {"same form"}, "issuance_token": {match[1]}}
	first := c.call("POST", "/tokens/project", form)
	if first.Code != 303 {
		t.Fatalf("first issue=%d", first.Code)
	}
	before := misuseSnapshot(t, s)
	second := c.call("POST", "/tokens/project", form)
	if second.Code != 303 && second.Code != 409 {
		t.Fatalf("replayed issue=%d", second.Code)
	}
	if misuseSnapshot(t, s) != before {
		t.Fatal("replaying the same issuance form created another token")
	}
}
func TestMisuseC7C8ApprovalReuseExpiryAndOwnership(t *testing.T) {
	for _, mode := range []string{"replay", "expired", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			s, _, h := misuseFixture(t)
			code := misuseDevice(t, s, "demo", "owner")
			owner := loginPanelUser(t, h, "owner")
			if mode == "expired" {
				now := time.Now().Add(11 * time.Minute)
				s.now = func() time.Time { return now }
			}
			if mode == "foreign" {
				owner = loginPanelUser(t, h, "outsider")
			}
			form := url.Values{"action": {"approve"}, "code": {code.UserCode}, "nickname": {"approved-agent"}}
			w := owner.call("POST", "/auth/code", form)
			if mode != "replay" {
				if w.Code != 404 {
					t.Errorf("approval %s status=%d", mode, w.Code)
				}
				if view, err := s.DeviceAuthorizationFor("owner", code.UserCode); err == nil && view.Status != "pending" {
					t.Fatal("unauthorized approval changed code")
				}
				return
			}
			if w.Code != 303 {
				t.Fatalf("first approval=%d", w.Code)
			}
			before := misuseSnapshot(t, s)
			for _, action := range []string{"approve", "deny"} {
				form.Set("action", action)
				w = owner.call("POST", "/auth/code", form)
				if w.Code != 409 {
					t.Errorf("reuse action=%s status=%d", action, w.Code)
				}
				if misuseSnapshot(t, s) != before {
					t.Fatal("processed code changed again")
				}
			}
		})
	}
}
func TestMisuseC8CodeGuessingIsBounded(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		t.Run(method, func(t *testing.T) {
			s, _, h := misuseFixture(t)
			c := loginPanelUser(t, h, "owner")
			before := misuseSnapshot(t, s)
			for i := 0; i < 11; i++ {
				path := "/auth/code"
				form := url.Values{"code": {"ZZZZ-ZZZZ"}, "action": {"lookup"}}
				if method == "GET" {
					path += "?code=ZZZZ-ZZZZ"
				}
				w := c.call(method, path, form)
				want := 404
				if i == 10 {
					want = 429
				}
				if w.Code != want {
					t.Errorf("guess %d status=%d want=%d", i, w.Code, want)
				}
			}
			if misuseSnapshot(t, s) != before {
				t.Fatal("code guessing mutated stored state")
			}
		})
	}
}
func TestMisuseC7DeleteTwice(t *testing.T) {
	s, b, h := misuseFixture(t)
	c := loginPanelUser(t, h, "owner")
	form := url.Values{"project": {"demo"}, "confirmName": {"demo"}}
	first := c.call("POST", "/projects/delete", form)
	if first.Code != 303 {
		t.Fatalf("first delete=%d", first.Code)
	}
	before := misuseSnapshot(t, s)
	second := c.call("POST", "/projects/delete", form)
	if second.Code != 403 && second.Code != 404 {
		t.Fatalf("second delete=%d", second.Code)
	}
	if len(b.mutations) != 1 || misuseSnapshot(t, s) != before {
		t.Fatal("delete replay reached backend or changed state")
	}
}

func TestMisuseC5MalformedIdentifiers(t *testing.T) {
	s, b, h := misuseFixture(t)
	c := loginPanelUser(t, h, "admin")
	before := misuseSnapshot(t, s)
	for _, id := range []string{"", "missing", "../other", "/absolute", "\x00", strings.Repeat("x", 2048)} {
		for _, path := range []string{"/admin/users/detail", "/admin/bug-reports/detail"} {
			w := c.call("GET", path+"?id="+url.QueryEscape(id), nil)
			if w.Code != 400 && w.Code != 404 {
				t.Errorf("%s invalid id status=%d", path, w.Code)
			}
		}
		for _, path := range []string{"/sessions/detach", "/sessions/suspend", "/sessions/unsuspend", "/tokens/revoke", "/admin/users/update", "/admin/bug-reports/status"} {
			form := url.Values{"project": {"demo"}, "id": {id}}
			if path == "/admin/bug-reports/status" {
				form = url.Values{"id": {id}, "status": {"resolved"}}
			}
			w := c.call("POST", path, form)
			if w.Code != 400 && w.Code != 403 && w.Code != 404 {
				t.Errorf("%s invalid id status=%d", path, w.Code)
			}
		}
		for _, path := range []string{"/projects/settings", "/projects/sessions", "/projects/sessions/live", "/projects/audit", "/projects/members/suggestions"} {
			w := c.call("GET", path+"?project="+url.QueryEscape(id), nil)
			if w.Code != 400 && w.Code != 403 && w.Code != 404 {
				t.Errorf("%s invalid project status=%d", path, w.Code)
			}
		}
	}
	assertMisuseUnchanged(t, s, b, before)
}

func TestMisuseC4CookieCannotAuthorizeBearerRoutes(t *testing.T) {
	_, _, h := misuseFixture(t)
	c := loginPanelUser(t, h, "admin")
	for _, path := range []string{deviceAuthorizationPath, deviceTokenPath, directPath, "/mcp"} {
		w := misuseRequest(h, c.cookie, http.MethodPost, path+"?project=demo", url.Values{"csrf": {c.csrf}})
		if w.Code != 400 && w.Code != 401 && w.Code != 403 {
			t.Errorf("cookie crossed bearer boundary %s status=%d", path, w.Code)
		}
	}
}

func TestMisuseC7TokenSubmissionConcurrentAndBound(t *testing.T) {
	s, _, h := misuseFixture(t)
	owner := loginPanelUser(t, h, "owner")
	other := loginPanelUser(t, h, "owner")
	body := owner.call("GET", "/projects/settings?project=demo", nil).Body.String()
	match := regexp.MustCompile(`name="issuance_token" value="([^"]+)"`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatal("issuance form nonce missing")
	}
	form := url.Values{"project": {"demo"}, "label": {"parallel"}, "issuance_token": {match[1]}}
	before := misuseSnapshot(t, s)
	if w := other.call("POST", "/tokens/project", form); w.Code != 409 {
		t.Fatalf("different login=%d", w.Code)
	}
	if misuseSnapshot(t, s) != before {
		t.Fatal("different login consumed issuance")
	}
	form.Set("csrf", owner.csrf)
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for range 8 {
		wg.Go(func() { codes <- misuseRequest(h, owner.cookie, "POST", "/tokens/project", form).Code })
	}
	wg.Wait()
	close(codes)
	successes := 0
	for code := range codes {
		if code == 303 {
			successes++
		} else if code != 409 {
			t.Errorf("parallel response=%d", code)
		}
	}
	if successes != 1 {
		t.Fatalf("issuance winners=%d", successes)
	}
	for _, p := range s.ProjectsFor("admin") {
		if p.Name == "demo" && len(p.Tokens) != 1 {
			t.Fatal("concurrent issuance duplicated token")
		}
	}
	// A new form with the same label is an intentional second issuance.
	if w := owner.call("POST", "/tokens/project", url.Values{"project": {"demo"}, "label": {"parallel"}}); w.Code != 303 {
		t.Fatal("fresh form cannot issue")
	}
}

func TestMisuseC7SubmissionExpiryProjectAndEmpty(t *testing.T) {
	p := &panel{}
	session := loginSession{csrf: "synthetic-session"}
	id := p.issueTokenSubmission(session, "demo")
	if p.takeTokenSubmission(session, "foreign", id) {
		t.Fatal("form crossed project boundary")
	}
	if !p.takeTokenSubmission(session, "demo", id) {
		t.Fatal("wrong project consumed original form")
	}
	id = p.issueTokenSubmission(session, "demo")
	entry := p.tokenSubmissions[id]
	entry.expires = time.Now().Add(-time.Second)
	p.tokenSubmissions[id] = entry
	if p.takeTokenSubmission(session, "demo", id) {
		t.Fatal("expired form accepted")
	}
	if p.takeTokenSubmission(session, "demo", "") {
		t.Fatal("missing form accepted")
	}
	for range 300 {
		p.issueTokenSubmission(session, "demo")
	}
	if len(p.tokenSubmissions) > 256 {
		t.Fatal("form cache is unbounded")
	}
	s, b, h := misuseFixture(t)
	c := loginPanelUser(t, h, "owner")
	before := misuseSnapshot(t, s)
	w := misuseRequest(h, c.cookie, "POST", "/tokens/project", url.Values{"csrf": {c.csrf}, "project": {"demo"}, "label": {"empty submission"}, "issuance_token": {""}})
	if w.Code != 409 {
		t.Fatalf("empty form status=%d", w.Code)
	}
	assertMisuseUnchanged(t, s, b, before)
}

func TestMisuseC4ExistingTokenAndPeerSessionIDs(t *testing.T) {
	s, b, h := misuseFixture(t)
	for _, seed := range []struct{ actor, project string }{{"owner", "demo"}, {"outsider", "foreign"}} {
		if _, err := s.IssueProjectTokenFor(seed.actor, seed.project, "boundary fixture"); err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]string{}
	for _, project := range s.ProjectsFor("admin") {
		if len(project.Tokens) > 0 {
			ids[project.Name] = project.Tokens[0].ID
		}
	}
	if ids["demo"] == "" || ids["foreign"] == "" {
		t.Fatal("missing fixture token")
	}
	before := misuseSnapshot(t, s)
	for _, tc := range []struct{ user, project, id string }{{"owner", "demo", ids["foreign"]}, {"developer", "demo", ids["demo"]}, {"viewer", "demo", ids["demo"]}} {
		c := loginPanelUser(t, h, tc.user)
		w := c.call("POST", "/tokens/revoke", url.Values{"project": {tc.project}, "id": {tc.id}})
		if w.Code != 400 && w.Code != 403 && w.Code != 404 {
			t.Fatalf("cross-boundary token status=%d", w.Code)
		}
	}
	for _, user := range []string{"developer", "viewer"} {
		c := loginPanelUser(t, h, user)
		for _, path := range []string{"/sessions/detach", "/sessions/suspend", "/sessions/unsuspend"} {
			w := c.call("POST", path, url.Values{"project": {"demo"}, "id": {"peer"}})
			if w.Code != 403 {
				t.Fatalf("peer session status=%d", w.Code)
			}
		}
	}
	assertMisuseUnchanged(t, s, b, before)
}
