package controlpanel

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func issueProjectResult(t *testing.T, c *userPanelClient, label string) string {
	t.Helper()
	w := c.call("POST", "/tokens/project", url.Values{"project": {"demo"}, "label": {label}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("issuance must redirect to GET, got %d", w.Code)
	}
	target, err := url.Parse(w.Header().Get("Location"))
	if err != nil || target.Path != "/projects/settings" || target.Query().Get("project") != "demo" || target.Query().Get("issued") == "" {
		t.Fatal("issuance result URL missing")
	}
	if strings.Contains(w.Header().Get("Location"), "asb_p_") || strings.Contains(w.Header().Get("Set-Cookie"), "asb_p_") || strings.Contains(w.Body.String(), "asb_p_") {
		t.Fatal("redirect exposes raw token")
	}
	return target.String()
}

func TestProjectTokenResultRefreshAndSessionBinding(t *testing.T) {
	s := userStore(t)
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	owner := loginPanelUser(t, h, "owner")
	otherLogin := loginPanelUser(t, h, "owner")
	admin := loginPanelUser(t, h, "admin")
	before := len(s.ProjectsFor("admin")[0].Tokens)
	target := issueProjectResult(t, owner, "refresh-test")
	for _, c := range []*userPanelClient{otherLogin, admin} {
		if strings.Contains(c.call("GET", target, nil).Body.String(), `id="issued-token"`) {
			t.Fatal("another login can read the token")
		}
	}
	first := owner.call("GET", target, nil)
	raw := issuedTokenFrom(t, first)
	if first.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("token response is cacheable")
	}
	for range 3 {
		refreshed := owner.call("GET", target, nil)
		if refreshed.Code != 200 || strings.Contains(refreshed.Body.String(), raw) || strings.Contains(refreshed.Body.String(), "프로젝트 토큰 발급 완료") {
			t.Fatal("refresh replays the one-time result")
		}
	}
	if len(s.ProjectsFor("admin")[0].Tokens) != before+1 {
		t.Fatal("refresh issued another token")
	}
}

func TestProjectTokenResultsStayWithTheirProjectAndTab(t *testing.T) {
	s := userStore(t)
	if err := s.CreateProjectFor("owner", "other"); err != nil {
		t.Fatal(err)
	}
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	c := loginPanelUser(t, h, "owner")
	first := issueProjectResult(t, c, "tab-one")
	second := issueProjectResult(t, c, "tab-two")
	wrong, _ := url.Parse(first)
	q := wrong.Query()
	q.Set("project", "other")
	wrong.RawQuery = q.Encode()
	if strings.Contains(c.call("GET", wrong.String(), nil).Body.String(), `id="issued-token"`) {
		t.Fatal("result leaked into another project")
	}
	rawSecond := issuedTokenFrom(t, c.call("GET", second, nil))
	rawFirst := issuedTokenFrom(t, c.call("GET", first, nil))
	if rawFirst == rawSecond {
		t.Fatal("tabs shared a result")
	}
}

func TestProjectTokenResultConcurrentReadIsSingleUse(t *testing.T) {
	s := userStore(t)
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	c := loginPanelUser(t, h, "owner")
	target := issueProjectResult(t, c, "concurrent")
	var wg sync.WaitGroup
	results := make(chan bool, 8)
	for range 8 {
		wg.Go(func() {
			r := httptest.NewRequest("GET", "http://broker"+target, nil)
			r.AddCookie(c.cookie)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			results <- strings.Contains(w.Body.String(), `id="issued-token"`)
		})
	}
	wg.Wait()
	close(results)
	count := 0
	for shown := range results {
		if shown {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("result displayed %d times", count)
	}
}

func TestProjectTokenResultExpiresAndStorageIsBounded(t *testing.T) {
	p := &panel{}
	session := loginSession{csrf: "test-login"}
	r := httptest.NewRequest("POST", "http://broker/tokens/project", nil)
	w := httptest.NewRecorder()
	p.redirectIssuedToken(w, r, session, "demo", "test-secret")
	target := w.Header().Get("Location")
	u, _ := url.Parse(target)
	id := u.Query().Get("issued")
	result := p.tokenResults[id]
	result.expires = time.Now().Add(-time.Second)
	p.tokenResults[id] = result
	get := httptest.NewRequest("GET", "http://broker"+target, nil)
	if p.takeIssuedToken(get, session, "demo") != "" || len(p.tokenResults) != 0 {
		t.Fatal("expired result was retained or displayed")
	}
	for range 257 {
		p.redirectIssuedToken(httptest.NewRecorder(), r, session, "demo", "test-secret")
	}
	if len(p.tokenResults) != 256 {
		t.Fatal("unbounded abandoned results")
	}
}
