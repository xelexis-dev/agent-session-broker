package controlpanel

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestAssetManifestMatchesEmbeddedFiles(t *testing.T) {
	if len(designAssets) < 129 {
		t.Fatalf("manifest lists %d assets", len(designAssets))
	}
	for path, asset := range designAssets {
		data, err := assets.ReadFile(asset.File)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != asset.SHA256 || int64(len(data)) != asset.Bytes {
			t.Errorf("%s does not match its manifest entry", path)
		}
		if !strings.HasPrefix(path, "/assets/") || strings.Contains(asset.File, "..") || asset.ContentType == "" || asset.License == "" {
			t.Errorf("unsafe or incomplete manifest entry %+v", asset)
		}
	}
}

func TestStaticAssetsServeOnlyListedFiles(t *testing.T) {
	h := New(userStore(t), &panelBackend{}, http.NotFoundHandler())
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://broker"+path, nil))
		return w
	}
	for path, want := range map[string]string{
		"/assets/panel.css": "text/css", "/assets/panel.js": "text/javascript", "/assets/design.css": "text/css",
		"/assets/theme.js": "text/javascript", "/assets/shell.js": "text/javascript", "/assets/tokens.css": "text/css",
		"/assets/icons/lucide-activity.svg": "image/svg+xml", "/assets/icons/lucide-menu.svg": "image/svg+xml",
	} {
		w := get(path)
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), want) || w.Body.Len() == 0 {
			t.Errorf("%s = %d %q", path, w.Code, w.Header().Get("Content-Type"))
		}
	}
	for _, path := range []string{"/assets/", "/assets/templates.html", "/assets/http.go", "/assets/asset-manifest.json", "/assets/icons/", "/assets/icons/../http.go", "/assets/fonts/unknown.woff2", "/assets/fonts.css", "/assets/fonts/roboto-mono-latin-400-normal.woff2", "/assets/icons/lucide-activity.svg/"} {
		if w := get(path); w.Code == 200 {
			t.Errorf("%s was served", path)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "http://broker/assets/panel.css", nil))
	if w.Code == 200 {
		t.Error("POST served an asset")
	}
}

func TestUnknownAssetBeforeLoginReturnsNotFound(t *testing.T) {
	h := New(userStore(t), &panelBackend{}, http.NotFoundHandler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://broker/assets/fonts/unknown.woff2", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown asset = %d location=%q, want 404", w.Code, w.Header().Get("Location"))
	}
}

func TestUnknownAssetsNeverRedirectOrSetCookies(t *testing.T) {
	unconfigured, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unconfigured.Close() })
	for _, state := range []struct {
		name  string
		store *Store
		login bool
	}{
		{"before setup", unconfigured, false},
		{"before login", userStore(t), false},
		{"after login", userStore(t), true},
	} {
		t.Run(state.name, func(t *testing.T) {
			h := New(state.store, &panelBackend{}, http.NotFoundHandler())
			var cookie *http.Cookie
			if state.login {
				cookie = loginPanelUser(t, h, "owner").cookie
			}
			for _, path := range []string{"/assets/", "/assets/templates.html", "/assets/http.go", "/assets/asset-manifest.json", "/assets/icons/", "/assets/icons/../http.go", "/assets/fonts/unknown.woff2", "/assets/fonts.css", "/assets/fonts/roboto-mono-latin-400-normal.woff2", "/assets/icons/lucide-activity.svg/"} {
				for _, method := range []string{http.MethodGet, http.MethodHead} {
					req := httptest.NewRequest(method, "http://broker"+path, nil)
					if cookie != nil {
						req.AddCookie(cookie)
					}
					w := httptest.NewRecorder()
					h.ServeHTTP(w, req)
					if w.Code != http.StatusNotFound || w.Header().Get("Location") != "" || len(w.Header().Values("Set-Cookie")) != 0 {
						t.Errorf("%s %s = %d location=%q cookie count=%d", method, path, w.Code, w.Header().Get("Location"), len(w.Header().Values("Set-Cookie")))
					}
				}
			}
		})
	}
}

func TestListedAssetHEADMatchesGETWithoutBody(t *testing.T) {
	h := New(userStore(t), &panelBackend{}, http.NotFoundHandler())
	for _, path := range []string{"/assets/panel.css?v=test", "/assets/panel.js", "/assets/icons/lucide-activity.svg"} {
		get, head := httptest.NewRecorder(), httptest.NewRecorder()
		h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "http://broker"+path, nil))
		h.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "http://broker"+path, nil))
		if get.Code != http.StatusOK || head.Code != http.StatusOK || get.Body.Len() == 0 || head.Body.Len() != 0 {
			t.Fatalf("%s GET/HEAD = %d/%d bytes=%d/%d", path, get.Code, head.Code, get.Body.Len(), head.Body.Len())
		}
		for _, header := range []string{"Content-Type", "Content-Length", "Cache-Control", "Content-Security-Policy"} {
			if head.Header().Get(header) != get.Header().Get(header) {
				t.Errorf("%s HEAD %s differs from GET", path, header)
			}
		}
	}
}

func TestSetupAndLoginSurviveUnknownAssetRequests(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ts := httptest.NewServer(New(s, &panelBackend{}, http.NotFoundHandler()))
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar} // Follow redirects to expose CSRF cookie rotation.
	noFollow := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base, _ := url.Parse(ts.URL)
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	for _, path := range []string{"/setup", "/login"} {
		res, err := noFollow.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		match := csrf.FindStringSubmatch(string(body))
		if res.StatusCode != http.StatusOK || len(match) != 2 {
			t.Fatalf("GET %s = %d without CSRF", path, res.StatusCode)
		}
		before := jar.Cookies(base)
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			req, _ := http.NewRequest(method, ts.URL+"/assets/fonts/unknown.woff2", nil)
			asset, err := browser.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, asset.Body)
			asset.Body.Close()
			if asset.StatusCode != http.StatusNotFound || len(asset.Cookies()) != 0 {
				t.Fatalf("%s unknown asset after %s = %d cookie count=%d", method, path, asset.StatusCode, len(asset.Cookies()))
			}
			if after := jar.Cookies(base); !samePanelCookies(before, after) {
				t.Fatalf("%s changed browser cookies after %s", method, path)
			}
		}
		form := url.Values{"csrf": {match[1]}, "password": {testPassword}}
		if path == "/setup" {
			form.Set("setupCode", setupCodeForTest(t, s))
			form.Set("confirmPassword", testPassword)
		} else {
			form.Set("username", "admin")
		}
		res, err = noFollow.PostForm(ts.URL+path, form)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST %s after unknown assets = %d", path, res.StatusCode)
		}
	}
}

func samePanelCookies(a, b []*http.Cookie) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Value != b[i].Value {
			return false
		}
	}
	return true
}

func TestContentSecurityPolicyAllowsOnlySameOriginFontsAndImages(t *testing.T) {
	h := New(userStore(t), &panelBackend{}, http.NotFoundHandler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://broker/login", nil))
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "style-src 'self'", "script-src 'self'", "img-src 'self'", "font-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %s: %s", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe") || strings.Contains(csp, "http") || strings.Contains(csp, "data:") {
		t.Errorf("CSP is looser than same-origin: %s", csp)
	}
	body := w.Body.String()
	if strings.Contains(body, "style=") || strings.Contains(body, "<style") || strings.Contains(body, "<script>") {
		t.Error("login page uses inline style or script")
	}
}
