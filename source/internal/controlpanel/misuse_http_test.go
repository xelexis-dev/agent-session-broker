package controlpanel

import (
	"context"
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

var misuseRoutes = map[string]string{
	"/profile/language": "post",
	"/admin/admin-api":  "post", "/profile/api-tokens": "page", "/profile/api-tokens/new": "page", "/profile/api-tokens/revoke": "post", "/profile/api-tokens/revoke-all": "post", "/admin/users/api-tokens/revoke": "post", "/admin/users/api-tokens/revoke-all": "post",
	"/api/":        "user-bearer",
	"/attachments": "bearer", "/attachments/": "bearer", "/admin/attachments": "post",
	"/healthz": "upstream", "/mcp": "bearer", "/oauth/transport-policy": "public-read", "/oauth/capabilities": "public-read", "/oauth/session_registration": "bearer", "/oauth/device_authorization": "bearer", "/oauth/token": "bearer", "/assets/": "asset", "/favicon.ico": "asset", "/setup": "access", "/login": "access",
	"/": "page", "/projects/settings": "page", "/projects/sessions": "page", "/projects/sessions/live": "json", "/projects/audit": "page", "/projects/members/suggestions": "json", "/admin/users": "page", "/admin/users/detail": "page", "/admin/settings": "page", "/profile": "page", "/admin/bug-reports": "page", "/admin/bug-reports/detail": "page", "/admin/announcement/result": "page",
	"/auth/code": "approval", "/projects/delete": "post", "/admin/announcement": "post", "/admin/settings/save": "post", "/profile/save": "post", "/profile/password": "post", "/admin/scope-issuance": "post", "/admin/http-policy": "post", "/projects/create": "post", "/projects/save": "post", "/projects/members": "post", "/tokens/project": "post", "/tokens/revoke": "post", "/tokens/scopes": "post", "/sessions/detach": "post", "/sessions/suspend": "post", "/sessions/unsuspend": "post", "/admin/users/create": "post", "/admin/users/update": "post", "/logout": "post", "/admin/bug-reports/status": "post", "/admin/bug-reports/delete": "post",
}

// Adding a mux branch requires an explicit browser/API boundary classification.
func TestMisuseRouteInventory(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "http.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	routeValues := panelRouteValues(t)
	seen := map[string]bool{directPath: true, capabilityPath: true, deviceAuthorizationPath: true, deviceTokenPath: true, deviceVerificationPath: true}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "serve" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				if value, found := routeValues[id.Name]; found {
					seen[value] = true
				}
			}
			lit, ok := n.(*ast.BasicLit)
			if ok && lit.Kind == token.STRING {
				s, _ := strconv.Unquote(lit.Value)
				if strings.HasPrefix(s, "/") {
					seen[s] = true
				}
			}
			return true
		})
	}
	for path := range seen {
		if _, ok := misuseRoutes[path]; !ok {
			t.Errorf("unclassified route %s", path)
		}
	}
	for path := range misuseRoutes {
		if !seen[path] {
			t.Errorf("stale route %s", path)
		}
	}
}

type misuseBackend struct {
	rows      map[string][]Session
	mutations []string
	reports   *bugreport.Store
}

func (b *misuseBackend) Sessions(_ context.Context, p string) ([]Session, error) {
	return b.rows[p], nil
}
func (b *misuseBackend) Detach(_ context.Context, p, id string) error {
	b.mutations = append(b.mutations, "detach:"+p+":"+id)
	return nil
}
func (b *misuseBackend) SetSuspended(_ context.Context, p, id string, on bool) error {
	b.mutations = append(b.mutations, "suspend:"+p+":"+id)
	return nil
}
func (b *misuseBackend) DeleteProject(_ context.Context, p string) error {
	b.mutations = append(b.mutations, "delete:"+p)
	return nil
}
func (b *misuseBackend) BugReports() *bugreport.Store { return b.reports }
func misuseFixture(t *testing.T) (*Store, *misuseBackend, http.Handler) {
	t.Helper()
	s := userStore(t)
	if err := s.CreateProjectFor("outsider", "foreign"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProjectFor("outsider", "foreign", "token", true); err != nil {
		t.Fatal(err)
	}
	reports, err := bugreport.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reports.Close() })
	reports.SetProjectPolicy(s.BugReportPolicy)
	b := &misuseBackend{reports: reports, rows: map[string][]Session{"demo": {{ID: "own", Name: "own-agent", OwnerID: "developer", Harness: "claude", SessionID: "own-conversation", ClientID: "own-pc"}, {ID: "peer", Name: "peer-agent", OwnerID: "owner", Harness: "claude", SessionID: "peer-conversation", ClientID: "peer-pc"}}, "foreign": {{ID: "foreign-id", Name: "foreign-agent", OwnerID: "outsider", Harness: "claude", SessionID: "foreign-conversation", ClientID: "foreign-pc"}}}}
	return s, b, New(s, b, http.NotFoundHandler())
}
func misuseRequest(h http.Handler, cookie *http.Cookie, method, path string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://broker"+path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func misuseSnapshot(t *testing.T, s *Store) string {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	raw, err := json.Marshal(s.data)
	if err != nil {
		t.Fatal("snapshot encoding failed")
	}
	return string(raw)
}
func assertMisuseUnchanged(t *testing.T, s *Store, b *misuseBackend, before string) {
	t.Helper()
	if misuseSnapshot(t, s) != before || len(b.mutations) != 0 {
		t.Fatal("rejected request mutated persistent or backend state")
	}
}

func TestMisuseC1SetupCannotResetAdmin(t *testing.T) {
	s, b, h := misuseFixture(t)
	before := misuseSnapshot(t, s)
	for _, method := range []string{"GET", "POST"} {
		for _, signedIn := range []bool{false, true} {
			var cookie *http.Cookie
			if signedIn {
				cookie = loginPanelUser(t, h, "admin").cookie
			}
			w := misuseRequest(h, cookie, method, "/setup", url.Values{"password": {"replacement test password"}, "confirmPassword": {"replacement test password"}, "csrf": {"synthetic-form"}})
			if w.Code != 409 {
				t.Errorf("%s signedIn=%t status=%d", method, signedIn, w.Code)
			}
		}
	}
	assertMisuseUnchanged(t, s, b, before)
	if _, ok := s.AuthenticateUser("admin", testPassword); !ok {
		t.Fatal("setup changed existing password")
	}
}
func TestMisuseC3EveryBrowserPostRequiresCSRF(t *testing.T) {
	s, b, h := misuseFixture(t)
	c := loginPanelUser(t, h, "admin")
	before := misuseSnapshot(t, s)
	for path, kind := range misuseRoutes {
		if kind != "post" && kind != "page" && kind != "approval" {
			continue
		}
		for _, bad := range []string{"", "wrong"} {
			t.Run(path+"/"+bad, func(t *testing.T) {
				w := misuseRequest(h, c.cookie, "POST", path, url.Values{"csrf": {bad}, "project": {"demo"}, "id": {"own"}, "confirmName": {"demo"}})
				if w.Code != 403 {
					t.Errorf("CSRF rejection=%d", w.Code)
				}
				assertMisuseUnchanged(t, s, b, before)
			})
		}
	}
	for _, path := range []string{"/setup", "/login"} {
		blank, err := OpenSQLite(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer blank.Close()
		if path == "/login" {
			if err := blank.Setup(testPassword); err != nil {
				t.Fatal(err)
			}
		}
		handler := New(blank, b, http.NotFoundHandler())
		get := misuseRequest(handler, nil, "GET", path, nil)
		cookie := get.Result().Cookies()[0]
		for _, bad := range []string{"", "wrong"} {
			w := misuseRequest(handler, cookie, "POST", path, url.Values{"csrf": {bad}, "password": {testPassword}, "confirmPassword": {testPassword}})
			if w.Code != 403 {
				t.Errorf("%s CSRF=%d", path, w.Code)
			}
		}
	}
}
func TestMisuseC3MutationGETAndLoggedOutPOST(t *testing.T) {
	s, b, h := misuseFixture(t)
	c := loginPanelUser(t, h, "admin")
	before := misuseSnapshot(t, s)
	for path, kind := range misuseRoutes {
		if kind != "post" {
			continue
		}
		t.Run(path, func(t *testing.T) {
			w := c.call("GET", path+"?project=demo&id=own&confirmName=demo", nil)
			if w.Code != 405 {
				t.Errorf("mutation GET=%d", w.Code)
			}
			w = misuseRequest(h, nil, "POST", path, url.Values{"csrf": {c.csrf}, "project": {"demo"}})
			if w.Code != 303 || w.Header().Get("Location") != "/login" {
				t.Errorf("logged-out POST=%d", w.Code)
			}
			assertMisuseUnchanged(t, s, b, before)
		})
	}
}
func TestMisuseC2LoginRedirectAndLimits(t *testing.T) {
	_, _, h := misuseFixture(t)
	for _, next := range []string{"https://evil.example", "//evil.example", "/\\evil.example", "/%2f%2fevil.example", "/login?next=https://evil.example", "/projects/delete?project=demo", "/profile%0d%0aLocation:evil", "/profile#evil", strings.Repeat("a", 5000)} {
		get := misuseRequest(h, nil, "GET", "/login", nil)
		cookie := get.Result().Cookies()[0]
		w := misuseRequest(h, cookie, "POST", "/login", url.Values{"csrf": {cookie.Value}, "username": {"admin"}, "password": {testPassword}, "next": {next}})
		if w.Code != 303 || w.Header().Get("Location") != "/" {
			t.Errorf("unsafe redirect status=%d", w.Code)
		}
	}
	_, _, h = misuseFixture(t)
	get := misuseRequest(h, nil, "GET", "/login", nil)
	cookie := get.Result().Cookies()[0]
	for i := 0; i < 11; i++ {
		password := "wrong"
		if i == 0 {
			password = strings.Repeat("x", 1025)
		}
		w := misuseRequest(h, cookie, "POST", "/login", url.Values{"csrf": {cookie.Value}, "username": {"admin"}, "password": {password}})
		want := 401
		if i == 10 {
			want = 429
		}
		if w.Code != want {
			t.Errorf("attempt %d status=%d", i, w.Code)
		}
	}
}
func TestMisuseC4AdminAndProjectBoundaries(t *testing.T) {
	s, b, h := misuseFixture(t)
	for _, user := range []string{"owner", "developer", "viewer", "outsider"} {
		c := loginPanelUser(t, h, user)
		for path, kind := range misuseRoutes {
			if !strings.HasPrefix(path, "/admin/") {
				continue
			}
			method := "GET"
			if kind == "post" {
				method = "POST"
			}
			t.Run(user+path, func(t *testing.T) {
				form := url.Values{}
				if method == "POST" {
					form = url.Values{"id": {"admin"}, "status": {"resolved"}, "enabled": {"true"}, "role": {"admin"}}
				}
				if path == "/admin/http-policy" {
					form = url.Values{"allowHTTP": {"true"}}
				}
				w := c.call(method, path, form)
				if w.Code != 403 {
					t.Errorf("admin boundary=%d", w.Code)
				}
			})
		}
	}
	c := loginPanelUser(t, h, "owner")
	before := misuseSnapshot(t, s)
	for _, path := range []string{"/projects/settings", "/projects/sessions", "/projects/sessions/live", "/projects/audit", "/projects/members/suggestions"} {
		w := c.call("GET", path+"?project=foreign", nil)
		if w.Code != 403 {
			t.Errorf("%s foreign GET=%d", path, w.Code)
		}
	}
	for _, path := range []string{"/projects/save", "/projects/members", "/tokens/project", "/tokens/revoke", "/sessions/detach", "/sessions/suspend", "/sessions/unsuspend", "/projects/delete"} {
		form := url.Values{"project": {"foreign"}, "id": {"foreign-id"}}
		if path == "/projects/delete" {
			form = url.Values{"project": {"foreign"}, "confirmName": {"foreign"}}
		}
		w := c.call("POST", path, form)
		if w.Code != 403 {
			t.Errorf("%s foreign POST=%d", path, w.Code)
		}
	}
	for _, path := range []string{"/sessions/detach", "/sessions/suspend", "/sessions/unsuspend"} {
		w := c.call("POST", path, url.Values{"project": {"demo"}, "id": {"foreign-id"}})
		if w.Code != 403 {
			t.Errorf("%s cross-project id=%d", path, w.Code)
		}
	}
	assertMisuseUnchanged(t, s, b, before)
}
func TestMisuseC9EveryBrowserPostBoundsBody(t *testing.T) {
	s, b, h := misuseFixture(t)
	c := loginPanelUser(t, h, "admin")
	before := misuseSnapshot(t, s)
	for path, kind := range misuseRoutes {
		if kind != "post" && kind != "access" && kind != "approval" {
			continue
		}
		t.Run(path, func(t *testing.T) {
			w := misuseRequest(h, c.cookie, "POST", path, url.Values{"csrf": {c.csrf}, "huge": {strings.Repeat("x", 48<<10)}})
			if w.Code != 400 {
				t.Errorf("oversize form=%d", w.Code)
			}
			assertMisuseUnchanged(t, s, b, before)
		})
	}
	if got := c.call("GET", "/profile", nil); got.Code != 200 {
		t.Fatal("handler unhealthy after oversized forms")
	}
}

// ResponseRecorder has no network parser: use an isolated httptest server.
func TestMisuseC9OversizeHeader(t *testing.T) {
	_, _, h := misuseFixture(t)
	server := httptest.NewServer(h)
	defer server.Close()
	req, err := http.NewRequest("GET", server.URL+"/login", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Misuse", strings.Repeat("x", 2<<20))
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal("oversize header request failed before response")
	}
	resp.Body.Close()
	if resp.StatusCode != 431 {
		t.Fatalf("header rejection=%d", resp.StatusCode)
	}
	resp, err = client.Get(server.URL + "/login")
	if err != nil {
		t.Fatal("server unhealthy after oversized header")
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("login no longer available")
	}
}
