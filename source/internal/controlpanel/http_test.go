package controlpanel

import (
	"context"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

type panelBackend struct{ detached string }

func (b *panelBackend) Sessions(context.Context, string) ([]Session, error) {
	return []Session{{ID: "session-1", Name: "home-test", Status: "missing", Harness: "codex", PID: 123}}, nil
}
func (b *panelBackend) Detach(_ context.Context, project, id string) error {
	b.detached = project + "/" + id
	return nil
}

func TestPanelSetupLoginPolicyAndLogout(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	backend := &panelBackend{}
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	ts := httptest.NewTLSServer(New(s, backend, downstream))
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Transport: ts.Client().Transport, Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string) (int, string) {
		t.Helper()
		r, e := client.Get(ts.URL + path)
		if e != nil {
			t.Fatal(e)
		}
		defer r.Body.Close()
		data, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(data)
	}
	post := func(path string, values url.Values) int {
		t.Helper()
		r, e := client.PostForm(ts.URL+path, values)
		if e != nil {
			t.Fatal(e)
		}
		defer r.Body.Close()
		return r.StatusCode
	}
	csrf := func(body string) string {
		t.Helper()
		m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(body)
		if len(m) != 2 {
			t.Fatalf("missing csrf form: %s", body)
		}
		return m[1]
	}
	if status, _ := get("/mcp?project=open"); status != 503 {
		t.Fatalf("pre-setup MCP = %d", status)
	}
	status, body := get("/setup")
	if status != 200 {
		t.Fatal(status)
	}
	token := csrf(body)
	if code := post("/setup", url.Values{"password": {testPassword}}); code != 403 {
		t.Fatalf("csrf missing = %d", code)
	}
	if code := post("/setup", url.Values{"setupCode": {setupCodeForTest(t, s)}, "password": {testPassword}, "confirmPassword": {testPassword}, "csrf": {token}}); code != 303 {
		t.Fatalf("setup = %d", code)
	}
	if code := post("/setup", url.Values{"password": {testPassword}, "confirmPassword": {testPassword}, "csrf": {token}}); code != 409 {
		t.Fatalf("second setup = %d", code)
	}
	_, body = get("/login")
	token = csrf(body)
	if code := post("/login", url.Values{"username": {"admin"}, "password": {"wrong"}, "csrf": {token}}); code != 401 {
		t.Fatalf("bad login = %d", code)
	}
	if code := post("/login", url.Values{"username": {"admin"}, "password": {testPassword}, "csrf": {token}}); code != 303 {
		t.Fatalf("login = %d", code)
	}
	status, body = get("/")
	if status != 200 || !strings.Contains(body, "프로젝트") {
		t.Fatalf("dashboard = %d", status)
	}
	token = csrf(body)
	if code := post("/projects/create", url.Values{"name": {"open"}, "mode": {"open"}, "enabled": {"true"}, "csrf": {token}}); code != 303 {
		t.Fatalf("project save = %d", code)
	}
	if status, _ := get("/mcp?project=open"); status != 401 {
		t.Fatalf("unauthenticated MCP = %d", status)
	}
	if status, _ := get("/mcp?project=unknown"); status != 404 {
		t.Fatalf("unknown MCP = %d", status)
	}
	if code := post("/sessions/detach", url.Values{"project": {"open"}, "id": {"session-1"}, "csrf": {token}}); code != 303 {
		t.Fatalf("detach = %d", code)
	}
	if backend.detached != "open/session-1" {
		t.Fatal("detach target lost")
	}
	if code := post("/logout", url.Values{"csrf": {token}}); code != 303 {
		t.Fatalf("logout = %d", code)
	}
	if status, _ := get("/"); status != 303 {
		t.Fatalf("logged out dashboard = %d", status)
	}
}

func TestPanelProtectsEveryProjectRequest(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProject("private", "token", true, ""); err != nil {
		t.Fatal(err)
	}
	testToken, err := s.IssueToken("private", "agent")
	if err != nil {
		t.Fatal(err)
	}
	h := New(s, &panelBackend{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for _, method := range []string{"GET", "POST", "DELETE"} {
		for _, token := range []string{"", "wrong", testToken} {
			req := httptest.NewRequest(method, "https://broker/mcp?project=private", nil)
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			response := httptest.NewRecorder()
			h.ServeHTTP(response, req)
			want := 401
			if token == testToken {
				want = 204
			}
			if response.Code != want {
				t.Errorf("%s token valid=%v: %d", method, token == testToken, response.Code)
			}
		}
	}
	req := httptest.NewRequest("POST", "http://broker/setup", strings.NewReader("password="+testPassword))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	if response.Code != 403 {
		t.Fatalf("cross-origin setup = %d", response.Code)
	}
}

func TestMCPGrantAndPolicyCancellation(t *testing.T) {
	for _, action := range []string{"revoke", "disable"} {
		t.Run(action, func(t *testing.T) {
			s, err := OpenSQLite(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err = s.Setup(testPassword); err != nil {
				t.Fatal(err)
			}
			if err = s.SaveProject("private", "token", true, ""); err != nil {
				t.Fatal(err)
			}
			raw, err := s.IssueToken("private", "reserved-name")
			if err != nil {
				t.Fatal(err)
			}
			entered := make(chan access.Grant, 1)
			done := make(chan struct{})
			h := New(s, &panelBackend{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				g, ok := access.FromContext(r.Context())
				if !ok {
					entered <- access.Grant{}
					return
				}
				entered <- g
				<-r.Context().Done()
			}))
			req := httptest.NewRequest("GET", "https://broker/mcp?project=private", nil)
			req.Header.Set("Authorization", "Bearer "+raw)
			go func() { defer close(done); h.ServeHTTP(httptest.NewRecorder(), req) }()
			var g access.Grant
			select {
			case g = <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("handler not entered")
			}
			if g.Nickname != "reserved-name" || g.Project != "private" || g.Claim == nil {
				t.Fatal("middleware did not attach grant")
			}
			switch action {
			case "revoke":
				err = s.RevokeToken("private", g.TokenID)
			case "disable":
				err = s.SaveProject("private", "token", false, "")
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("policy change did not cancel request")
			}
			if g.Claim("pc/session") == nil {
				t.Fatal("cancelled grant accepted")
			}
		})
	}
}

func TestTokenUIIssueOnceRevokeAndCreateSafety(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProject("private", "token", true, ""); err != nil {
		t.Fatal(err)
	}
	h := New(s, &panelBackend{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	// Authenticate through real login flow and carry its cookies and CSRF value.
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest("GET", "http://broker/login", nil))
	csrfRE := regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	csrf := csrfRE.FindStringSubmatch(get.Body.String())[1]
	req := httptest.NewRequest("POST", "http://broker/login", strings.NewReader(url.Values{"csrf": {csrf}, "username": {"admin"}, "password": {testPassword}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(get.Result().Cookies()[0])
	login := httptest.NewRecorder()
	h.ServeHTTP(login, req)
	if login.Code != 303 {
		t.Fatal("login failed", login.Code)
	}
	cookie := login.Result().Cookies()[0]
	call := func(method, path string, v url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "http://broker"+path, strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	dashboard := call("GET", "/projects/settings?project=private", nil)
	csrf = csrfRE.FindStringSubmatch(dashboard.Body.String())[1]
	if w := call("POST", "/tokens/project", url.Values{"project": {"private"}, "label": {"agent"}}); w.Code != 403 {
		t.Fatal("issue without CSRF", w.Code)
	}
	submission := regexp.MustCompile(`name="issuance_token" value="([^"]+)"`).FindStringSubmatch(dashboard.Body.String())[1]
	issued := call("POST", "/tokens/project", url.Values{"project": {"private"}, "label": {"agent"}, "csrf": {csrf}, "issuance_token": {submission}})
	if issued.Code != 303 || issued.Header().Get("Cache-Control") != "no-store" || issued.Header().Get("Location") == "" {
		t.Fatal("issuance response", issued.Code)
	}
	issued = call("GET", issued.Header().Get("Location"), nil)
	match := regexp.MustCompile(`<code id="issued-token">([^<]+)</code>`).FindStringSubmatch(issued.Body.String())
	if len(match) != 2 || !strings.HasPrefix(match[1], "asb_p_") {
		t.Fatal("missing one-time project token")
	}
	projectToken := match[1]
	raw, err := s.IssueToken("private", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Authorize("private", raw) || s.Authorize("private", projectToken) {
		t.Fatal("token authorization mismatch")
	}
	refreshed := call("GET", "/projects/settings?project=private", nil)
	if strings.Contains(refreshed.Body.String(), projectToken) || !strings.Contains(refreshed.Body.String(), ">agent<") {
		t.Fatal("token leaked or label missing")
	}
	if w := call("POST", "/projects/create", url.Values{"name": {"private"}, "csrf": {csrf}}); w.Code != 400 || s.Authorize("private", "") {
		t.Fatal("create overwrote policy", w.Code)
	}
	if w := call("POST", "/tokens/issue", url.Values{"project": {"private"}, "nickname": {"agent"}, "csrf": {csrf}}); w.Code != 404 {
		t.Fatal("legacy issuance route still served", w.Code)
	}
	var projectID, legacyID string
	for _, token := range s.Projects()[0].Tokens {
		if token.Kind == tokenProject {
			projectID = token.ID
		} else {
			legacyID = token.ID
		}
	}
	if w := call("POST", "/tokens/revoke", url.Values{"project": {"private"}, "id": {projectID}}); w.Code != 403 {
		t.Fatal("revoke without CSRF", w.Code)
	}
	if w := call("POST", "/tokens/revoke", url.Values{"project": {"private"}, "id": {projectID}, "csrf": {csrf}}); w.Code != 303 || w.Header().Get("Location") != "/projects/settings?project=private" {
		t.Fatal("revoke failed", w.Code)
	}
	refreshed = call("GET", "/projects/settings?project=private", nil)
	if strings.Contains(refreshed.Body.String(), `value="`+projectID+`"`) || strings.Contains(refreshed.Body.String(), "취소됨") || !strings.Contains(refreshed.Body.String(), "표시할 프로젝트 토큰이 없습니다.") {
		t.Fatal("revoked token remains in the UI list")
	}
	// Legacy tokens stay revocable by id even though the UI no longer lists them.
	if w := call("POST", "/tokens/revoke", url.Values{"project": {"private"}, "id": {legacyID}, "csrf": {csrf}}); w.Code != 303 {
		t.Fatal("legacy revoke failed", w.Code)
	}
	for _, method := range []string{"GET", "POST", "DELETE"} {
		req := httptest.NewRequest(method, "https://broker/mcp?project=private", nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 401 {
			t.Fatal("revoked request reached MCP", method, w.Code)
		}
	}
}
