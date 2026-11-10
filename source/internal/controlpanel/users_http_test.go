package controlpanel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

type userPanelClient struct {
	t      *testing.T
	h      http.Handler
	cookie *http.Cookie
	csrf   string
}

func loginPanelUser(t *testing.T, h http.Handler, username string) *userPanelClient {
	t.Helper()
	c := &userPanelClient{t: t, h: h}
	get := c.call("GET", "/login", nil)
	match := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(get.Body.String())
	if len(match) != 2 {
		t.Fatal("login CSRF missing")
	}
	c.cookie = get.Result().Cookies()[0]
	c.csrf = match[1]
	login := c.call("POST", "/login", url.Values{"username": {username}, "password": {testPassword}})
	if login.Code != 303 {
		t.Fatalf("%s login = %d", username, login.Code)
	}
	c.cookie = login.Result().Cookies()[0]
	home := c.call("GET", "/", nil)
	match = regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(home.Body.String())
	if len(match) != 2 {
		t.Fatal("authenticated CSRF missing")
	}
	c.csrf = match[1]
	return c
}
func (c *userPanelClient) call(method, path string, values url.Values) *httptest.ResponseRecorder {
	c.t.Helper()
	copyValues := url.Values{}
	for key, entries := range values {
		copyValues[key] = append([]string(nil), entries...)
	}
	values = copyValues
	if method == "POST" && path == "/tokens/project" && values.Get("issuance_token") == "" {
		form := c.call("GET", projectURL("settings", values.Get("project")), nil)
		match := regexp.MustCompile(`name="issuance_token" value="([^"]+)"`).FindStringSubmatch(form.Body.String())
		if len(match) == 2 {
			values.Set("issuance_token", match[1])
		}
	}
	if method == "POST" {
		values.Set("csrf", c.csrf)
	}
	req := httptest.NewRequest(method, "http://broker"+path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, req)
	return w
}

type ownedPanelBackend struct{ detached []string }

func (b *ownedPanelBackend) Sessions(context.Context, string) ([]Session, error) {
	return []Session{{ID: "owned", SessionID: "sid", ClientID: "pc", Harness: "codex", Name: "dev-session"}, {ID: "other", SessionID: "other-sid", ClientID: "pc", Harness: "codex", Name: "admin-session"}}, nil
}
func (b *ownedPanelBackend) Detach(_ context.Context, project, id string) error {
	b.detached = append(b.detached, project+"/"+id)
	return nil
}

func TestUserPanelRolesAndSessionOwnership(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"owner", "dev", "viewer", "outsider"} {
		if err = s.CreateUser("admin", id, testPassword, "user"); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.CreateProjectFor("owner", "demo"); err != nil {
		t.Fatal(err)
	}
	for id, role := range map[string]string{"dev": "developer", "viewer": "viewer"} {
		if err = s.SetProjectMember("owner", "demo", id, role); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.SaveProjectFor("owner", "demo", "token", true); err != nil {
		t.Fatal(err)
	}
	raw, err := s.IssueTokenFor("dev", "demo", "dev-session", "dev")
	if err != nil {
		t.Fatal(err)
	}
	grant, _, ok := s.authorize("demo", raw)
	if !ok {
		t.Fatal("grant denied")
	}
	if err = grant.Claim(`["codex","sid","pc"]`); err != nil {
		t.Fatal(err)
	}
	b := &ownedPanelBackend{}
	h := New(s, b, http.NotFoundHandler())
	viewer := loginPanelUser(t, h, "viewer")
	for _, path := range []string{"/projects/settings?project=demo", "/projects/sessions?project=demo"} {
		if w := viewer.call("GET", path, nil); w.Code != 200 {
			t.Fatalf("viewer GET %s = %d", path, w.Code)
		}
	}
	for _, path := range []string{"/tokens/project", "/tokens/revoke", "/projects/save", "/projects/members", "/sessions/detach", "/admin/users/create", "/admin/users/update"} {
		if w := viewer.call("POST", path, url.Values{"project": {"demo"}, "name": {"demo"}, "id": {"owned"}, "nickname": {"blocked"}, "username": {"outsider"}, "role": {"owner"}}); w.Code != 403 {
			t.Errorf("viewer POST %s = %d", path, w.Code)
		}
	}
	if len(b.detached) != 0 {
		t.Fatal("denied detach reached backend")
	}
	outsider := loginPanelUser(t, h, "outsider")
	if w := outsider.call("GET", "/", nil); strings.Contains(w.Body.String(), ">demo<") {
		t.Fatal("outsider sees project")
	}
	for _, path := range []string{"/projects/settings?project=demo", "/projects/sessions?project=demo", "/admin/users"} {
		if w := outsider.call("GET", path, nil); w.Code != 403 {
			t.Errorf("outsider GET = %d", w.Code)
		}
	}
	if w := outsider.call("POST", "/projects/create", url.Values{"name": {"mine"}}); w.Code != 303 || s.ProjectRole("outsider", "mine") != "owner" {
		t.Fatal("creator not owner")
	}
	dev := loginPanelUser(t, h, "dev")
	if w := dev.call("POST", "/tokens/project", url.Values{"project": {"demo"}, "label": {"mine"}, "ownerID": {"owner"}}); w.Code != 303 {
		t.Fatalf("developer project token = %d", w.Code)
	}
	if tokens := s.ProjectsFor("admin")[0].Tokens; tokens[len(tokens)-1].OwnerID != "dev" {
		t.Fatal("owner field overrode the actor")
	}
	if w := dev.call("POST", "/sessions/detach", url.Values{"project": {"demo"}, "id": {"other"}}); w.Code != 403 {
		t.Fatalf("other session detach = %d", w.Code)
	}
	if w := dev.call("POST", "/sessions/detach", url.Values{"project": {"demo"}, "id": {"owned"}}); w.Code != 303 {
		t.Fatalf("own session detach = %d", w.Code)
	}
	if len(b.detached) != 1 || b.detached[0] != "demo/owned" {
		t.Fatal("detach target incorrect")
	}
	owner := loginPanelUser(t, h, "owner")
	if w := owner.call("POST", "/projects/members", url.Values{"project": {"demo"}, "username": {"outsider"}, "role": {"owner"}}); w.Code != 303 || s.ProjectRole("outsider", "demo") != "owner" {
		t.Fatal("owner membership update denied")
	}
	if err = s.UpdateUser("admin", "dev", "user", true, "changed-password-123"); err != nil {
		t.Fatal(err)
	}
	if w := dev.call("GET", "/", nil); w.Code != 303 || w.Header().Get("Location") != "/login?next=%2F" {
		t.Fatal("changed user retained login")
	}
}

func TestUserPanelPagesModalAndAdministration(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateProjectFor("admin", "demo"); err != nil {
		t.Fatal(err)
	}
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	admin := loginPanelUser(t, h, "admin")
	home := admin.call("GET", "/", nil)
	for _, want := range []string{`<dialog`, `data-dialog-open="create-project"`, `/assets/panel.js`, `/projects/sessions?project=demo`} {
		if !strings.Contains(home.Body.String(), want) {
			t.Errorf("home missing %s", want)
		}
	}
	if strings.Contains(home.Body.String(), `action="/projects/save"`) || strings.Contains(home.Body.String(), `action="/tokens/project"`) {
		t.Fatal("list includes project details")
	}
	if !strings.Contains(home.Header().Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatal("external script not allowed")
	}
	if w := admin.call("GET", "/?project=demo", nil); w.Code != 303 || w.Header().Get("Location") != "/projects/sessions?project=demo" {
		t.Fatal("legacy detail redirect missing")
	}
	settings := admin.call("GET", "/projects/settings?project=demo", nil)
	if !strings.Contains(settings.Body.String(), `action="/projects/save"`) || !strings.Contains(settings.Body.String(), `/projects/sessions?project=demo`) {
		t.Fatal("project settings navigation missing")
	}
	if w := admin.call("POST", "/projects/create", url.Values{"name": {"Bad Name"}}); w.Code != 400 || !strings.Contains(w.Body.String(), `data-dialog-reopen="create-project"`) {
		t.Fatal("invalid create lost modal")
	}
	if w := admin.call("POST", "/admin/users/create", url.Values{"username": {"new-user"}, "password": {testPassword}, "role": {"user"}}); w.Code != 303 {
		t.Fatalf("create user = %d", w.Code)
	}
	if w := admin.call("GET", "/admin/users", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "new-user") {
		t.Fatal("user management missing")
	}
	if w := admin.call("POST", "/admin/users/update", url.Values{"id": {"new-user"}, "role": {"user"}}); w.Code != 303 {
		t.Fatalf("disable user = %d", w.Code)
	}
	if u, ok := s.User("new-user"); !ok || u.Enabled {
		t.Fatal("user not disabled")
	}
}

func TestTokenPanelRegistrationAndEligibleOwners(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"owner", "inactive"} {
		if err = s.CreateUser("admin", id, testPassword, "user"); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.CreateProjectFor("owner", "demo"); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProjectFor("owner", "demo", "token", true); err != nil {
		t.Fatal(err)
	}
	if err = s.SetProjectMember("owner", "demo", "inactive", "developer"); err != nil {
		t.Fatal(err)
	}
	if err = s.UpdateUser("admin", "inactive", "user", false, ""); err != nil {
		t.Fatal(err)
	}
	req := DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "sid", InstanceID: "pc", RequestID: "req-1"}
	raw, err := s.IssueProjectTokenFor("owner", "demo", "pending-registration")
	if err != nil {
		t.Fatal(err)
	}
	issued, err := s.BeginDeviceAuthorization("demo", raw, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApproveDeviceAuthorization("owner", issued.UserCode, "pending-nick", false); err != nil {
		t.Fatal(err)
	}
	granted, err := s.ExchangeDeviceToken("demo", raw, issued.DeviceCode, "asb-client-v1", false)
	if err != nil {
		t.Fatal(err)
	}
	grant, _, ok := s.authorize("demo", granted.AccessToken)
	if !ok {
		t.Fatal("grant denied")
	}
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	admin := loginPanelUser(t, h, "admin")
	w := admin.call("GET", "/projects/settings?project=demo", nil)
	if strings.Contains(w.Body.String(), `name="ownerID"`) {
		t.Error("token owner must be fixed to the issuer")
	}
	if strings.Contains(w.Body.String(), ">등록 완료</span>") {
		t.Error("approval alone displayed as completed registration")
	}
	if err = grant.Connected(req.identity()); err != nil {
		t.Fatal(err)
	}
	w = admin.call("GET", "/projects/settings?project=demo", nil)
	if !strings.Contains(w.Body.String(), ">등록 완료</span>") {
		t.Error("completed registration not displayed")
	}
}
