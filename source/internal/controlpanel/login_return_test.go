package controlpanel

import (
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestLoginReturnsToRequestedProjectPage(t *testing.T) {
	s := userStore(t)
	if err := s.CreateProjectFor("admin", "x"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(s, &panelBackend{}, http.NotFoundHandler()))
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(server.URL + "/projects/sessions?project=x")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 303 {
		t.Fatalf("protected page status = %d", response.StatusCode)
	}
	loginURL := response.Header.Get("Location")
	response, err = client.Get(server.URL + loginURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	field := func(name string) string {
		m := regexp.MustCompile(`name="` + name + `" value="([^"]*)"`).FindSubmatch(body)
		if len(m) != 2 {
			return ""
		}
		return html.UnescapeString(string(m[1]))
	}
	response, err = client.PostForm(server.URL+"/login", url.Values{"username": {"admin"}, "password": {testPassword}, "csrf": {field("csrf")}, "next": {field("next")}})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 303 || response.Header.Get("Location") != "/projects/sessions?project=x" {
		t.Fatalf("login return = %d %q, want 303 /projects/sessions?project=x", response.StatusCode, response.Header.Get("Location"))
	}
	page, err := client.Get(server.URL + "/projects/sessions?project=x")
	if err != nil {
		t.Fatal(err)
	}
	page.Body.Close()
	if page.StatusCode != 200 {
		t.Fatalf("returned page = %d", page.StatusCode)
	}
}

func TestLoginNextAllowsOnlyCanonicalPanelViews(t *testing.T) {
	for _, target := range []string{
		"/", "/?project=demo", "/projects/sessions?project=x&filter=a%20b",
		"/projects/settings?project=x", "/admin/users", "/admin/users/detail?id=alice",
		"/admin/settings", "/profile", "/auth/code?code=fixture", "/admin/announcement/result?id=fixture",
		"/profile?label=%ED%95%9C%EA%B8%80&label=two", "/profile?example=https%3A%2F%2Fexample.org%2F",
	} {
		t.Run(target, func(t *testing.T) {
			if got := safeLoginNext(target); got != target {
				t.Fatalf("next %q = %q", target, got)
			}
		})
	}
	for _, target := range []string{
		"", "profile", "https://example.org/profile", "http://localhost/profile", "javascript:alert(1)", "//example.org/profile",
		"///profile", "\\example.org\\profile", "/\\example.org", "/profile?value=\\x", "/profile?value=%5cx",
		"/%2fexample.org", "/%252fexample.org", "/projects%2fsessions?project=x", "/%70rofile", "/profile%2f..%2flogin",
		"/profile/../login", "/profile/", "/login", "/login?next=/profile", "/setup", "/assets/panel.js", "/favicon.ico",
		"/mcp", "/oauth/transport-policy", "/projects/members/suggestions", "/sessions/detach", "/admin/settings/save", "/unknown",
		"/profile#anchor", "/profile#", "/profile?bad=%", "/profile?bad=%0d%0aLocation%3a%20https%3a%2f%2fexample.org",
		"/profile?bad=%00", "/profile\r\n", "/profile\t", " /profile", "/profile?space=raw value",
		"/profile?long=" + strings.Repeat("x", 4096),
	} {
		t.Run(target, func(t *testing.T) {
			if got := safeLoginNext(target); got != "" {
				t.Fatalf("unsafe next %q accepted as %q", target, got)
			}
		})
	}
}

func TestLoginReturnRetryAndInvalidPostFallback(t *testing.T) {
	s := userStore(t)
	for _, tc := range []struct{ name, next, want string }{
		{"query preserved", "/admin/users/detail?id=owner&tab=access", "/admin/users/detail?id=owner&tab=access"},
		{"direct login", "", "/"}, {"external", "//example.org", "/"},
		{"encoded path", "/%2fexample.org", "/"}, {"self", "/login", "/"}, {"asset", "/assets/panel.css", "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(New(s, &panelBackend{}, http.NotFoundHandler()))
			defer server.Close()
			jar, _ := cookiejar.New(nil)
			client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			get, err := client.Get(server.URL + "/login?next=" + url.QueryEscape(tc.next))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(get.Body)
			get.Body.Close()
			m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindSubmatch(body)
			if len(m) != 2 {
				t.Fatal("missing CSRF field")
			}
			form := url.Values{"username": {"admin"}, "password": {"wrong"}, "csrf": {string(m[1])}, "next": {tc.next}}
			failed, err := client.PostForm(server.URL+"/login", form)
			if err != nil {
				t.Fatal(err)
			}
			body, _ = io.ReadAll(failed.Body)
			failed.Body.Close()
			if failed.StatusCode != 401 {
				t.Fatalf("retry status = %d", failed.StatusCode)
			}
			if tc.want != "/" && !strings.Contains(html.UnescapeString(string(body)), `name="next" value="`+tc.next+`"`) {
				t.Fatal("401 lost the validated return target")
			}
			form.Set("password", testPassword)
			response, err := client.PostForm(server.URL+"/login", form)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 303 || response.Header.Get("Location") != tc.want {
				t.Fatalf("login = %d %q, want303 %q", response.StatusCode, response.Header.Get("Location"), tc.want)
			}
		})
	}
}

func TestLoginReturnDoesNotReplayPostsOrAmbiguousNext(t *testing.T) {
	for _, tc := range []struct{ method, target, want string }{
		{"GET", "/admin/settings", "/login?next=%2Fadmin%2Fsettings"},
		{"GET", "/projects/members/suggestions?project=x", "/login"},
		{"GET", "/unknown", "/login"}, {"POST", "/sessions/detach", "/login"},
	} {
		r := httptest.NewRequest(tc.method, tc.target, nil)
		if got := loginLocation(r); got != tc.want {
			t.Fatalf("%s %s => %q, want%q", tc.method, tc.target, got, tc.want)
		}
	}
	r := httptest.NewRequest("GET", "/login?next=/profile&next=//example.org", nil)
	if got := loginNext(r); got != "" {
		t.Fatalf("duplicate next accepted: %q", got)
	}
	r = httptest.NewRequest("POST", "/login?next=/profile", nil)
	r.PostForm = url.Values{"next": {"/admin/settings"}}
	if got := loginNext(r); got != "/admin/settings" {
		t.Fatalf("query overrides POST: %q", got)
	}
	delete(r.PostForm, "next")
	if got := loginNext(r); got != "" {
		t.Fatalf("POST query used as next: %q", got)
	}
}

func TestLoginReturnKeepsCSRFAndRateLimit(t *testing.T) {
	s := userStore(t)
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest("GET", "/login?next=%2Fprofile", nil))
	var cookie *http.Cookie
	for _, c := range get.Result().Cookies() {
		if c.Name == "asb_form" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("missing form cookie")
	}
	post := func(csrf string) int {
		form := url.Values{"username": {"admin"}, "password": {"wrong"}, "csrf": {csrf}, "next": {"/profile"}}
		r := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Header().Get("Content-Security-Policy") != get.Header().Get("Content-Security-Policy") {
			t.Fatal("login return changed CSP")
		}
		return w.Code
	}
	if got := post("invalid"); got != 403 {
		t.Fatalf("bad CSRF = %d", got)
	}
	for i := 0; i < 10; i++ {
		if got := post(cookie.Value); got != 401 {
			t.Fatalf("attempt %d = %d", i+1, got)
		}
	}
	if got := post(cookie.Value); got != 429 {
		t.Fatalf("login rate limit = %d", got)
	}
}
