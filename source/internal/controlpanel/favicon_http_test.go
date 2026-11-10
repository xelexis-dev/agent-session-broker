package controlpanel

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// Browsers request /favicon.ico on their own. It used to be redirected to
// /setup or /login, whose GET issued a new asb_form cookie and broke the open
// form's CSRF value.
func TestFaviconNeverRedirectsOrSetsCookies(t *testing.T) {
	unconfigured, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unconfigured.Close() })
	for name, store := range map[string]*Store{"before setup": unconfigured, "signed out": userStore(t)} {
		h := New(store, &panelBackend{}, http.NotFoundHandler())
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(method, "http://broker/favicon.ico", nil))
			if w.Code != http.StatusNoContent || w.Header().Get("Location") != "" || len(w.Result().Cookies()) != 0 {
				t.Errorf("%s %s /favicon.ico = %d location=%q cookies=%d", name, method, w.Code, w.Header().Get("Location"), len(w.Result().Cookies()))
			}
		}
	}
}

func TestSetupAndLoginSurviveTheBrowserFaviconRequest(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ts := httptest.NewServer(New(s, &panelBackend{}, http.NotFoundHandler()))
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar} // follows redirects like a browser's favicon fetch
	noFollow := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	openForm := func(path string) string {
		t.Helper()
		res, err := noFollow.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		m := csrf.FindStringSubmatch(string(body))
		if res.StatusCode != 200 || m == nil {
			t.Fatalf("GET %s = %d without csrf", path, res.StatusCode)
		}
		if res, err := browser.Get(ts.URL + "/favicon.ico"); err == nil {
			res.Body.Close()
		}
		return m[1]
	}
	submit := func(path string, form url.Values) int {
		t.Helper()
		res, err := noFollow.PostForm(ts.URL+path, form)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	token := openForm("/setup")
	if code := submit("/setup", url.Values{"setupCode": {setupCodeForTest(t, s)}, "csrf": {token}, "password": {testPassword}, "confirmPassword": {testPassword}}); code != http.StatusSeeOther {
		t.Fatalf("setup after favicon = %d", code)
	}
	token = openForm("/login")
	if code := submit("/login", url.Values{"csrf": {token}, "username": {"admin"}, "password": {testPassword}}); code != http.StatusSeeOther {
		t.Fatalf("login after favicon = %d", code)
	}
}

func TestPagesDeclareTheirIcon(t *testing.T) {
	const link = `<link rel="icon" type="image/svg+xml" href="/assets/icons/lucide-command.svg">`
	if _, ok := designAssets["/assets/icons/lucide-command.svg"]; !ok {
		t.Fatal("icon is not a served asset")
	}
	h := New(userStore(t), &panelBackend{}, http.NotFoundHandler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://broker/login", nil))
	if !strings.Contains(w.Body.String(), link) {
		t.Error("login page does not declare its icon")
	}
	if body := loginPanelUser(t, h, "owner").call("GET", "/", nil).Body.String(); !strings.Contains(body, link) {
		t.Error("panel page does not declare its icon")
	}
}
