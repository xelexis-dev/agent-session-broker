package controlpanel

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func panelUserToken(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	match := regexp.MustCompile(`asb_u_[A-Z2-7]{52}`).FindString(w.Body.String())
	if match == "" {
		t.Fatal("one-time user token missing")
	}
	return match
}
func TestPanelUserTokenLifecycle(t *testing.T) {
	s, h := apiFixture(t)
	if err := s.SetInstanceHTTPPolicy("admin", true); err != nil {
		t.Fatal(err)
	}
	client := loginPanelUser(t, h, "admin")
	issued := client.call("POST", "/profile/api-tokens", url.Values{"label": {"synthetic"}, "current_password": {testPassword}, "expires_days": {"30"}, "scope": {"read", "write"}})
	if issued.Code != 303 {
		t.Fatalf("issue=%d", issued.Code)
	}
	location := issued.Header().Get("Location")
	first := client.call("GET", location, nil)
	raw := panelUserToken(t, first)
	if strings.Contains(client.call("GET", location, nil).Body.String(), raw) {
		t.Fatal("receipt replay")
	}
	if w := apiRequest(h, "GET", "/api/v1/me", "", raw); w.Code != 200 {
		t.Fatalf("bearer=%d", w.Code)
	}
	// Resolve only public metadata to revoke the exact newly issued token.
	var id string
	for _, token := range s.api.tokens {
		if token.Hash == digest(raw) {
			id = token.ID
		}
	}
	if w := client.call("POST", "/profile/api-tokens/revoke", url.Values{"id": {id}}); w.Code != 303 {
		t.Fatalf("revoke=%d", w.Code)
	}
	if w := apiRequest(h, "GET", "/api/v1/me", "", raw); w.Code != 401 {
		t.Fatalf("revoked bearer=%d", w.Code)
	}
}

func panelAPIRequest(c *userPanelClient, method, path string, values url.Values, secure bool) *httptest.ResponseRecorder {
	form := url.Values{}
	for k, v := range values {
		form[k] = append([]string(nil), v...)
	}
	if method == "POST" {
		form.Set("csrf", c.csrf)
	}
	scheme := "https"
	if !secure {
		scheme = "http"
	}
	r := httptest.NewRequest(method, scheme+"://broker"+path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(c.cookie)
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, r)
	return w
}
func panelIssueForm() url.Values {
	return url.Values{"scope": {"read", "write"}, "current_password": {testPassword}, "expires_days": {"30"}}
}
func TestPanelAPIReceiptKindsAndHTTP(t *testing.T) {
	s, h := apiFixture(t)
	h = New(s, &panelBackend{}, http.NotFoundHandler())
	if err := s.CreateProjectFor("admin", "admin"); err != nil {
		t.Fatal(err)
	}
	client := loginPanelUser(t, h, "admin")
	issue := panelAPIRequest(client, "POST", "/profile/api-tokens", panelIssueForm(), true)
	if issue.Code != 303 {
		t.Fatalf("HTTPS issue=%d", issue.Code)
	}
	u, _ := url.Parse(issue.Header().Get("Location"))
	id := u.Query().Get("issued")
	wrong := panelAPIRequest(client, "GET", "/projects/settings?project=admin&issued="+id, nil, true)
	if strings.Contains(wrong.Body.String(), "asb_u_") {
		t.Fatal("user receipt consumed by project route")
	}
	userRaw := panelUserToken(t, panelAPIRequest(client, "GET", u.String(), nil, true))
	if userRaw == "" {
		t.Fatal("user result")
	}
	proj := client.call("POST", "/tokens/project", url.Values{"project": {"admin"}, "label": {"kind"}})
	target, _ := url.Parse(proj.Header().Get("Location"))
	if strings.Contains(panelAPIRequest(client, "GET", "/profile/api-tokens?issued="+target.Query().Get("issued"), nil, true).Body.String(), "asb_p_") {
		t.Fatal("project receipt consumed by profile")
	}
	projectResult := client.call("GET", target.String(), nil)
	if !strings.Contains(projectResult.Body.String(), "asb_p_") {
		t.Fatal("project receipt policy changed")
	}
	issue = panelAPIRequest(client, "POST", "/profile/api-tokens", panelIssueForm(), true)
	location := issue.Header().Get("Location")
	denied := panelAPIRequest(client, "GET", location, nil, false)
	if strings.Contains(denied.Body.String(), "asb_u_") {
		t.Fatal("HTTP receipt leaked")
	}
	if strings.Contains(panelAPIRequest(client, "GET", location, nil, true).Body.String(), "asb_u_") {
		t.Fatal("refused receipt was not dropped")
	}
	denied = panelAPIRequest(client, "POST", "/profile/api-tokens", panelIssueForm(), false)
	if denied.Code != 403 || !strings.Contains(denied.Body.String(), `data-api-error="http"`) {
		t.Fatal("HTTP issue not refused")
	}
}
func TestPanelAPITokenErrorsAndOffIssuance(t *testing.T) {
	s, h := apiFixture(t)
	client := loginPanelUser(t, h, "admin")
	for _, stale := range []bool{false, true} {
		form := panelIssueForm()
		form.Del("current_password")
		if stale {
			form.Set("current_password", "wrong synthetic password")
		}
		w := panelAPIRequest(client, "POST", "/profile/api-tokens", form, true)
		if w.Code != 303 {
			t.Fatalf("password-free issue stale=%v status=%d", stale, w.Code)
		}
	}
	if err := s.SetAdminAPI("admin", false); err != nil {
		t.Fatal(err)
	}
	w := panelAPIRequest(client, "POST", "/profile/api-tokens", panelIssueForm(), true)
	if w.Code != 303 {
		t.Fatalf("OFF panel issue=%d", w.Code)
	}
	raw := panelUserToken(t, panelAPIRequest(client, "GET", w.Header().Get("Location"), nil, true))
	if w = apiRequest(h, "GET", "/api/v1/me", "", raw); w.Code != 403 {
		t.Fatal("OFF API admitted")
	}
}
func TestPanelAPIAdminAndCSRF(t *testing.T) {
	s := userStore(t)
	if err := s.SetAdminAPI("admin", true); err != nil {
		t.Fatal(err)
	}
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	admin := loginPanelUser(t, h, "admin")
	owner := loginPanelUser(t, h, "owner")
	enabled, version, _ := s.adminAPIPolicySnapshot()
	if !enabled {
		t.Fatal("fixture")
	}
	w := owner.call("POST", "/admin/admin-api", url.Values{"enabled": {"false"}, "settings_version": {version}})
	if w.Code != 403 {
		t.Fatal("non-admin switch")
	}
	w = owner.call("POST", "/admin/users/api-tokens/revoke-all", url.Values{"user": {"admin"}})
	if w.Code != 403 {
		t.Fatal("non-admin revoke")
	}
	for _, path := range []string{"/admin/admin-api", "/profile/api-tokens", "/profile/api-tokens/revoke", "/profile/api-tokens/revoke-all", "/admin/users/api-tokens/revoke", "/admin/users/api-tokens/revoke-all"} {
		r := httptest.NewRequest("POST", "https://broker"+path, strings.NewReader("enabled=false"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(admin.cookie)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("CSRF %s=%d", path, w.Code)
		}
	}
	w = admin.call("POST", "/admin/admin-api", url.Values{"enabled": {"false"}, "settings_version": {version}})
	if w.Code != 303 {
		t.Fatalf("switch=%d", w.Code)
	}
	w = admin.call("POST", "/admin/admin-api", url.Values{"enabled": {"true"}, "settings_version": {version}})
	if w.Code != 409 {
		t.Fatal("stale switch version")
	}
	_, _, last := s.adminAPIPolicySnapshot()
	if last == nil || last.Actor != "admin" || !bool(last.Old) || bool(last.New) {
		t.Fatal("policy last change")
	}
	before := last.At
	if err := s.SetAdminAPI("admin", false); err != nil {
		t.Fatal(err)
	}
	_, _, last = s.adminAPIPolicySnapshot()
	if !last.At.Equal(before) {
		t.Fatal("no-op changed actor/time")
	}
	records, _ := s.listRecords()
	s.mu.Lock()
	s.loadAPI(records)
	s.mu.Unlock()
	_, _, last = s.adminAPIPolicySnapshot()
	if last == nil || !last.At.Equal(before) || s.AdminAPIEnabled() {
		t.Fatal("last change restore")
	}
}
func TestPanelAPIAdminRevokeMemoryFirstAndList(t *testing.T) {
	s := userStore(t)
	_ = s.SetAdminAPI("admin", true)
	owner, _ := s.User("owner")
	raw, view, err := s.IssueOwnUserToken("owner", owner.Revision, "owner token", 0, []string{"read"}, "synthetic-digest", false)
	if err != nil {
		t.Fatal(err)
	}
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	admin := loginPanelUser(t, h, "admin")
	page := admin.call("GET", "/admin/users/detail?id=owner", nil)
	if !strings.Contains(page.Body.String(), view.ID) || strings.Contains(page.Body.String(), `action="/profile/api-tokens"`) {
		t.Fatal("admin list/issuance disclosure")
	}
	write := s.writeBatch
	s.writeBatch = func(map[string][]byte, []string) error { return ErrStorage }
	w := admin.call("POST", "/admin/users/api-tokens/revoke", url.Values{"user": {"owner"}, "id": {view.ID}})
	if w.Code != 503 {
		t.Fatal("failed revoke persistence")
	}
	if _, err = s.authenticateAPI(raw); err == nil {
		t.Fatal("failed persistence kept token live")
	}
	s.writeBatch = write
	if err = s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	for _, event := range s.api.audits {
		if event.Action == "token_revoke" && event.Actor == "admin" {
			return
		}
	}
	// Failed audit persistence is represented by the failed operation, not fabricated later.
}

func TestPanelAPITokenCapAndAdmission(t *testing.T) {
	s, h := apiFixture(t)
	client := loginPanelUser(t, h, "admin")
	admin, _ := s.User("admin")
	called := false
	if _, _, err := s.IssueOwnUserToken("admin", admin.Revision, "", 0, []string{"read"}, "panel-principal", false, func() bool { called = true; return false }); err != errAPIRate || !called {
		t.Fatal("panel admission bypassed")
	}
	for range 32 {
		if _, _, err := s.IssueOwnUserToken("admin", admin.Revision, "", 0, []string{"read"}, "cap-fixture", false); err != nil {
			t.Fatal(err)
		}
	}
	w := panelAPIRequest(client, "POST", "/profile/api-tokens", panelIssueForm(), true)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `data-api-error="cap"`) {
		t.Fatalf("cap state=%d", w.Code)
	}
}

func TestPanelAPITokenSeparatePages(t *testing.T) {
	_, h := apiFixture(t)
	client := loginPanelUser(t, h, "admin")
	profile := panelAPIRequest(client, "GET", "/profile", nil, true)
	if strings.Contains(profile.Body.String(), `action="/profile/api-tokens"`) {
		t.Fatal("issue form remains on profile")
	}
	list := panelAPIRequest(client, "GET", "/profile/api-tokens", nil, true)
	if list.Code != 200 {
		t.Fatalf("token list=%d want200", list.Code)
	}
	if strings.Contains(list.Body.String(), `action="/profile/api-tokens"`) || !strings.Contains(list.Body.String(), `href="/profile/api-tokens/new"`) {
		t.Fatal("token list/new separation")
	}
	registration := panelAPIRequest(client, "GET", "/profile/api-tokens/new", nil, true)
	if registration.Code != 200 || !strings.Contains(registration.Body.String(), `action="/profile/api-tokens"`) {
		t.Fatal("registration form missing")
	}
	for _, page := range []*httptest.ResponseRecorder{profile, list, registration} {
		if !strings.Contains(page.Body.String(), `aria-label="`+en("profile.my_profile_menu")+`"`) {
			t.Fatal("profile submenu missing")
		}
	}
}

func TestPanelAPITokenIssuanceRejectsInvalidatedSession(t *testing.T) {
	for _, stage := range []string{"before-snapshot", "before-commit"} {
		t.Run(stage, func(t *testing.T) {
			s, _ := apiFixture(t)
			p := &panel{store: s, backend: &panelBackend{}, next: http.NotFoundHandler(), sessions: map[string]loginSession{}}
			client := loginPanelUser(t, http.HandlerFunc(p.serve), "admin")
			form := panelIssueForm()
			form.Del("current_password")
			form.Set("csrf", client.csrf)
			req := httptest.NewRequest("POST", "https://broker/profile/api-tokens", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.AddCookie(client.cookie)
			if err := req.ParseForm(); err != nil {
				t.Fatal("form fixture")
			}
			session, ok := p.session(req)
			if !ok || !equal(session.csrf, req.PostForm.Get("csrf")) {
				t.Fatal("session/CSRF fixture")
			}
			change := func() {
				if err := s.ChangeOwnPassword("admin", testPassword, "replacement synthetic password", "replacement synthetic password"); err != nil {
					t.Fatal("password change fixture")
				}
			}
			if stage == "before-snapshot" {
				change()
			} else {
				s.beforeTokenExchange = change
			}
			w := httptest.NewRecorder()
			p.profileAPITokensMutate(w, req, session)
			s.mu.RLock()
			count := len(s.api.tokens)
			s.mu.RUnlock()
			records, err := s.listRecords()
			if err != nil {
				t.Fatal("record fixture")
			}
			stored := 0
			for key := range records {
				if strings.HasPrefix(key, "user-tokens/") {
					stored++
				}
			}
			t.Logf("status=%d token_count=%d stored_count=%d", w.Code, count, stored)
			if w.Code == http.StatusSeeOther || count != 0 || stored != 0 {
				t.Fatal("invalidated session issued a token")
			}
		})
	}
}
