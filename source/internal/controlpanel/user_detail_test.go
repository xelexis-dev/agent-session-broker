package controlpanel

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func userDetailFixture(t *testing.T) (*Store, http.Handler, *userPanelClient) {
	t.Helper()
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("admin", "alice", testPassword, "user"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateProfile("alice", "Alice Example", "alice@example.test"); err != nil {
		t.Fatal(err)
	}
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	return s, h, loginPanelUser(t, h, "admin")
}

func TestUserDetailListAndReadOnlyProfile(t *testing.T) {
	_, _, admin := userDetailFixture(t)
	list := admin.call("GET", "/admin/users", nil).Body.String()
	for _, want := range []string{`id="user-table"`, `data-row-links`, `href="/admin/users/detail?id=alice"`, `Alice Example`, `data-table-filter="user-table"`} {
		if !strings.Contains(list, want) {
			t.Errorf("list missing %s", want)
		}
	}
	if strings.Contains(list, `action="/admin/users/update"`) {
		t.Error("list still contains inline editor")
	}
	w := admin.call("GET", "/admin/users/detail?id=alice", nil)
	if w.Code != 200 {
		t.Fatalf("detail status = %d", w.Code)
	}
	for _, want := range []string{`Alice Example`, `alice@example.test`, `action="/admin/users/update"`, `name="id" value="alice"`, `href="/admin/users"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("detail missing %s", want)
		}
	}
}

func TestUserDetailAuthorizationAndMissingID(t *testing.T) {
	_, h, admin := userDetailFixture(t)
	ordinary := loginPanelUser(t, h, "alice")
	for _, id := range []string{"alice", "unknown", ""} {
		if w := ordinary.call("GET", "/admin/users/detail?id="+id, nil); w.Code != 403 {
			t.Errorf("ordinary %q = %d", id, w.Code)
		}
	}
	for _, id := range []string{"unknown", ""} {
		if w := admin.call("GET", "/admin/users/detail?id="+id, nil); w.Code != 404 {
			t.Errorf("missing GET %q = %d", id, w.Code)
		}
		if w := admin.call("POST", "/admin/users/update", url.Values{"id": {id}, "role": {"user"}}); w.Code != 404 {
			t.Errorf("missing POST %q = %d", id, w.Code)
		}
	}
	for _, client := range []*userPanelClient{ordinary, admin} {
		if client == admin {
			client.csrf = "invalid"
		}
		if w := client.call("POST", "/admin/users/update", url.Values{"id": {"alice"}, "role": {"admin"}, "enabled": {"true"}}); w.Code != 403 {
			t.Errorf("unauthorized update = %d", w.Code)
		}
	}
}

func TestUserDetailFailurePreservesSafeFields(t *testing.T) {
	s, _, admin := userDetailFixture(t)
	w := admin.call("POST", "/admin/users/update", url.Values{"id": {"alice"}, "role": {"admin"}, "password": {"tiny-pass"}})
	if w.Code != 400 {
		t.Fatalf("invalid update = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`name="id" value="alice"`, `role="alert"`, `alice@example.test`} {
		if !strings.Contains(body, want) {
			t.Errorf("failed detail missing %s", want)
		}
	}
	if !regexp.MustCompile(`<option value="admin"\s+selected`).MatchString(body) {
		t.Error("submitted role lost")
	}
	if regexp.MustCompile(`<input[^>]*name="enabled"[^>]*checked`).MatchString(body) {
		t.Error("unchecked enabled value lost")
	}
	if strings.Contains(body, "tiny-pass") || regexp.MustCompile(`<input[^>]*type="password"[^>]*\svalue=`).MatchString(body) {
		t.Error("password echoed")
	}
	u, _ := s.User("alice")
	if u.Role != "user" || !u.Enabled {
		t.Error("invalid update changed account")
	}
}

func TestUserDetailSaveRedirectAndSessionInvalidation(t *testing.T) {
	s, h, admin := userDetailFixture(t)
	alice := loginPanelUser(t, h, "alice")
	w := admin.call("POST", "/admin/users/update", url.Values{"id": {"alice"}, "role": {"admin"}, "enabled": {"true"}})
	if w.Code != 303 || w.Header().Get("Location") != "/admin/users/detail?id=alice" {
		t.Fatalf("save redirect = %d %s", w.Code, w.Header().Get("Location"))
	}
	u, _ := s.User("alice")
	if u.Role != "admin" {
		t.Error("save did not update role")
	}
	if w := alice.call("GET", "/", nil); w.Code != 303 || w.Header().Get("Location") != "/login?next=%2F" {
		t.Error("edited user retained old login")
	}
	notice := w.Result().Cookies()
	if len(notice) == 0 {
		t.Fatal("success notice cookie missing")
	}
	req, _ := http.NewRequest("GET", "http://broker/admin/users/detail?id=alice", nil)
	req.AddCookie(admin.cookie)
	for _, c := range notice {
		req.AddCookie(c)
	}
	// Render the PRG destination with the actual one-time notice cookie.
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	if !strings.Contains(response.Body.String(), "사용자 정보를 저장했습니다.") {
		t.Error("detail success feedback missing")
	}
	alice = loginPanelUser(t, h, "alice")
	w = alice.call("POST", "/admin/users/update", url.Values{"id": {"alice"}, "role": {"user"}, "enabled": {"true"}})
	if w.Code != 303 || w.Header().Get("Location") != "/login" {
		t.Error("self role change did not require login")
	}
}

func TestUserDetailProtectedAdminAndLastOwner(t *testing.T) {
	s, _, admin := userDetailFixture(t)
	body := admin.call("GET", "/admin/users/detail?id=admin", nil).Body.String()
	for _, want := range []string{`type="hidden" name="role" value="admin"`, `type="hidden" name="enabled" value="true"`} {
		if !strings.Contains(body, want) {
			t.Errorf("protected admin missing %s", want)
		}
	}
	if w := admin.call("POST", "/admin/users/update", url.Values{"id": {"admin"}, "role": {"user"}}); w.Code != 400 {
		t.Error("fixed admin protection lost")
	}
	if err := s.CreateProjectFor("alice", "owned"); err != nil {
		t.Fatal(err)
	}
	if w := admin.call("POST", "/admin/users/update", url.Values{"id": {"alice"}, "role": {"user"}}); w.Code != 400 {
		t.Error("last active owner protection lost")
	}
	u, _ := s.User("alice")
	if !u.Enabled {
		t.Error("last owner disabled")
	}
}

// The new password needs a matching confirmation. Both empty keeps the
// password; any other mismatch (one side empty included) is rejected before
// anything is saved, and neither value is shown again.
func TestUserDetailPasswordConfirmation(t *testing.T) {
	s, h, admin := userDetailFixture(t)
	const fresh, other = "fresh password 1234", "other password 5678"
	before, _ := s.User("alice")
	for _, tc := range []struct{ name, password, confirm string }{
		{"mismatch", fresh, other},
		{"confirm empty", fresh, ""},
		{"password empty", "", fresh},
	} {
		w := admin.call("POST", "/admin/users/update", url.Values{"id": {"alice"}, "role": {"admin"}, "password": {tc.password}, "password_confirm": {tc.confirm}})
		if w.Code != 400 {
			t.Fatalf("%s = %d", tc.name, w.Code)
		}
		body := w.Body.String()
		if strings.Contains(body, fresh) || strings.Contains(body, other) || regexp.MustCompile(`<input[^>]*type="password"[^>]*\svalue=`).MatchString(body) {
			t.Errorf("%s echoed a password", tc.name)
		}
		if !strings.Contains(body, `role="alert"`) {
			t.Errorf("%s has no error message", tc.name)
		}
		after, _ := s.User("alice")
		if after != before {
			t.Errorf("%s changed the account: %+v", tc.name, after)
		}
		if _, ok := s.AuthenticateUser("alice", testPassword); !ok {
			t.Errorf("%s changed the password", tc.name)
		}
	}
	if w := admin.call("POST", "/admin/users/update", url.Values{"id": {"alice"}, "role": {"user"}, "enabled": {"true"}, "password": {""}, "password_confirm": {""}}); w.Code != 303 {
		t.Fatalf("both empty = %d", w.Code)
	}
	if _, ok := s.AuthenticateUser("alice", testPassword); !ok {
		t.Error("both empty changed the password")
	}
	if w := admin.call("POST", "/admin/users/update", url.Values{"id": {"alice"}, "role": {"user"}, "enabled": {"true"}, "password": {fresh}, "password_confirm": {fresh}}); w.Code != 303 || w.Header().Get("Location") != "/admin/users/detail?id=alice" {
		t.Fatalf("matching = %d %s", w.Code, w.Header().Get("Location"))
	}
	if _, ok := s.AuthenticateUser("alice", fresh); !ok {
		t.Error("matching password was not saved")
	}
	self := loginPanelUser(t, h, "admin")
	if w := self.call("POST", "/admin/users/update", url.Values{"id": {"admin"}, "role": {"admin"}, "enabled": {"true"}, "password": {fresh}, "password_confirm": {fresh}}); w.Code != 303 || w.Header().Get("Location") != "/login" {
		t.Errorf("own password change = %d %s, want re-login", w.Code, w.Header().Get("Location"))
	}
}

func TestUserDetailConfirmFieldAndActiveSwitch(t *testing.T) {
	_, _, admin := userDetailFixture(t)
	body := admin.call("GET", "/admin/users/detail?id=alice", nil).Body.String()
	for _, want := range []string{
		`<input type="password" name="password_confirm" aria-label="alice 새 비밀번호 확인" minlength="12" maxlength="1024" autocomplete="new-password"`,
		`<input type="checkbox" name="enabled" value="true" class="switch__input" role="switch" aria-label="alice 계정 활성" checked`,
		`<span class="switch__track" aria-hidden="true"><span class="switch__thumb"></span></span><span class="switch__label">계정 활성</span><span class="switch__state" aria-hidden="true"><span class="switch__on">켜짐</span><span class="switch__off">꺼짐</span></span>`,
		`<link rel="stylesheet" href="/assets/switch.css?v=`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail lacks %s", want)
		}
	}
	fixed := admin.call("GET", "/admin/users/detail?id=admin", nil).Body.String()
	if !regexp.MustCompile(`<input type="checkbox" name="enabled" value="true" class="switch__input" role="switch"[^>]*\sdisabled`).MatchString(fixed) || !strings.Contains(fixed, `<input type="hidden" name="enabled" value="true">`) {
		t.Error("fixed admin switch is not locked on")
	}
}
