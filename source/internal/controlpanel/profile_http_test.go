package controlpanel

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestProfileHTTPReadAndSelfUpdate(t *testing.T) {
	s := userStore(t)
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://broker/profile", nil))
	if w.Code != 303 || w.Header().Get("Location") != "/login?next=%2Fprofile" {
		t.Fatal("anonymous profile exposed")
	}
	c := loginPanelUser(t, h, "viewer")
	home := c.call("GET", "/", nil)
	if !strings.Contains(home.Body.String(), `href="/profile"`) {
		t.Fatal("profile navigation missing")
	}
	profile := c.call("GET", "/profile", nil)
	if profile.Code != 200 || !strings.Contains(profile.Body.String(), `action="/profile/save"`) || !strings.Contains(profile.Body.String(), `action="/profile/password"`) {
		t.Fatal("profile forms missing")
	}
	for _, markup := range []string{`<div class="profile-layout">`, ("<aside class=\"profile-account\" aria-label=\"" + en("profile.account") + "\">"), `<div class="profile-forms profile-content">`, ("<div class=\"profile-full profile-actions\"><button class=\"primary\">" + en("profile.save") + "</button></div>"), ("<div class=\"profile-full profile-actions\"><button class=\"primary\">" + en("profile.change_password") + "</button></div>"), ("<p class=\"profile-name\">viewer</p><p class=\"profile-meta\">" + en("tokens.user") + "</p>"), `</label><small id="display-name-help">`, `</label><small id="profile-password-help">`} {
		if !strings.Contains(profile.Body.String(), markup) {
			t.Fatalf("profile layout missing: %s", markup)
		}
	}
	for _, forbidden := range []string{`name="id"`, `name="role"`, `name="enabled"`, `name="members"`} {
		if strings.Contains(profile.Body.String(), forbidden) {
			t.Fatal("profile exposes privileged input")
		}
	}
	before, _ := s.User("viewer")
	w = c.call("POST", "/profile/save", url.Values{"displayName": {"표시 이름"}, "email": {"viewer@example.test"}})
	if w.Code != 303 || w.Header().Get("Location") != "/profile" {
		t.Fatalf("profile save = %d", w.Code)
	}
	after, _ := s.User("viewer")
	if after.DisplayName != "표시 이름" || after.Email != "viewer@example.test" || after.ID != before.ID || after.Role != before.Role || after.Revision != before.Revision {
		t.Fatal("incorrect profile change")
	}
	if got := c.call("GET", "/profile", nil); got.Code != 200 || (!strings.Contains(got.Body.String(), "viewer@example.test") || !strings.Contains(got.Body.String(), `<p class="profile-name">표시 이름</p><p class="profile-meta">viewer · `+en("tokens.user")+`</p>`)) {
		t.Fatal("saved profile not visible")
	}
}

func TestProfileHTTPProjectRolesRespectMembership(t *testing.T) {
	s := userStore(t)
	if err := s.CreateProjectFor("admin", "private-project"); err != nil {
		t.Fatal(err)
	}
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	for user, role := range map[string]string{"owner": "Owner", "developer": "Developer", "viewer": "Viewer"} {
		t.Run(user, func(t *testing.T) {
			c := loginPanelUser(t, h, user)
			profile := c.call("GET", "/profile", nil)
			body := profile.Body.String()
			if profile.Code != 200 || !strings.Contains(body, `href="/projects/settings?project=demo"`) || !strings.Contains(body, en("profile.project_memberships")+` <span class="profile-count">1</span>`) {
				t.Fatal("own project role or access link missing")
			}
			if !strings.Contains(body, `<li><a class="profile-project" href="/projects/settings?project=demo"><span class="profile-project__name">demo</span><span class="profile-role" data-role="`+role+`">`+role+`</span><span class="icon icon--arrow-right" aria-hidden="true"></span></a></li>`) || (strings.Contains(body, "View project settings") || strings.Contains(body, "프로젝트 설정 보기")) || strings.Contains(body, "profile-roles") || strings.Contains(body, "<dt>"+role) {
				t.Fatal("profile project item structure missing")
			}
			if strings.Contains(body, "private-project") {
				t.Fatal("profile exposes another user's project")
			}
			if got := c.call("GET", "/projects/settings?project=demo", nil); got.Code != 200 {
				t.Fatal("profile project link is not accessible")
			}
		})
	}
	for _, user := range []string{"outsider", "admin"} {
		t.Run(user, func(t *testing.T) {
			if user == "admin" {
				// An administrator can see all projects, but only their own memberships belong in the profile.
				if err := s.SetProjectMember("admin", "private-project", "owner", "owner"); err != nil {
					t.Fatal(err)
				}
				if err := s.SetProjectMember("admin", "private-project", "admin", ""); err != nil {
					t.Fatal(err)
				}
			}
			profile := loginPanelUser(t, h, user).call("GET", "/profile", nil)
			body := profile.Body.String()
			if profile.Code != 200 || !strings.Contains(body, `class="profile-membership-empty"`) {
				t.Fatal("empty membership state missing")
			}
			if strings.Contains(body, `href="/projects/settings?project=`) {
				t.Fatal("profile includes a project without membership")
			}
		})
	}
}

func TestProfileHTTPRejectsForgedFieldsCSRFAndInvalidInput(t *testing.T) {
	s := userStore(t)
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	c := loginPanelUser(t, h, "developer")
	before, _ := s.User("developer")
	admin, _ := s.User("admin")
	for _, field := range []string{"id", "userID", "username", "role", "enabled", "members", "projectRole"} {
		for _, path := range []string{"/profile/save", "/profile/password"} {
			w := c.call("POST", path, url.Values{field: {"admin"}, "displayName": {"forged"}})
			if w.Code != 403 {
				t.Errorf("forged %s on %s = %d", field, path, w.Code)
			}
		}
	}
	if w := c.call("POST", "/profile/save?id=admin", url.Values{"displayName": {"forged"}}); w.Code != 403 {
		t.Fatal("query target accepted")
	}
	if w := c.call("POST", "/profile/save", url.Values{"displayName": {"one", "two"}}); w.Code != 403 {
		t.Fatal("duplicate fields accepted")
	}
	for _, values := range []url.Values{{"displayName": {"bad\nname"}}, {"email": {"not-email"}}} {
		if w := c.call("POST", "/profile/save", values); w.Code != 400 {
			t.Fatal("invalid profile accepted")
		}
	}
	c.csrf = "wrong"
	if w := c.call("POST", "/profile/save", url.Values{"displayName": {"forged"}}); w.Code != 403 {
		t.Fatal("invalid CSRF accepted")
	}
	after, _ := s.User("developer")
	adminAfter, _ := s.User("admin")
	if after != before || adminAfter != admin {
		t.Fatal("denied request changed account")
	}
}

func TestProfileHTTPPasswordChangesInvalidateAllOldLogins(t *testing.T) {
	s := userStore(t)
	if err := s.UpdateProfile("developer", "Existing Name", "existing@example.test"); err != nil {
		t.Fatal(err)
	}
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	first := loginPanelUser(t, h, "developer")
	second := loginPanelUser(t, h, "developer")
	other := loginPanelUser(t, h, "viewer")
	next := "new profile login password for tests"
	for _, values := range []url.Values{{"currentPassword": {"incorrect-current-password"}, "newPassword": {next}, "confirmPassword": {next}}, {"currentPassword": {testPassword}, "newPassword": {next}, "confirmPassword": {"confirmation-mismatch"}}} {
		w := first.call("POST", "/profile/password", values)
		if w.Code != 400 {
			t.Fatal("invalid password accepted")
		}
		if !strings.Contains(w.Body.String(), `value="Existing Name"`) || !strings.Contains(w.Body.String(), `value="existing@example.test"`) {
			t.Fatal("password error emptied existing profile fields")
		}
		for _, secret := range []string{testPassword, next, "incorrect-current-password", "confirmation-mismatch"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("password reflected in response")
			}
		}
	}
	w := first.call("POST", "/profile/password", url.Values{"currentPassword": {testPassword}, "newPassword": {next}, "confirmPassword": {next}})
	if w.Code != 303 || w.Header().Get("Location") != "/login" {
		t.Fatal("password change did not require login")
	}
	for _, c := range []*userPanelClient{first, second} {
		if got := c.call("GET", "/profile", nil); got.Code != 303 || got.Header().Get("Location") != "/login?next=%2Fprofile" {
			t.Fatal("old login survived password change")
		}
	}
	if got := other.call("GET", "/profile", nil); got.Code != 200 {
		t.Fatal("other user's login invalidated")
	}
	if _, ok := s.AuthenticateUser("developer", next); !ok {
		t.Fatal("new password not accepted")
	}
	if _, ok := s.AuthenticateUser("developer", testPassword); ok {
		t.Fatal("old password accepted")
	}
}

func TestProfileProjects(t *testing.T) {
	projects := []Project{
		{Name: "zeta", Members: map[string]string{"u": "owner"}},
		{Name: "alpha", Members: map[string]string{"u": "viewer"}},
		{Name: "mid", Members: map[string]string{"u": "custom-role"}},
		{Name: "other", Members: map[string]string{"other": "owner"}},
	}
	want := []profileProject{{"alpha", "Viewer"}, {"mid", "custom-role"}, {"zeta", "Owner"}}
	if got := profileProjects(projects, "u"); !reflect.DeepEqual(got, want) {
		t.Fatalf("projects = %v, want %v", got, want)
	}
	if got := profileProjects(projects, "absent"); len(got) != 0 {
		t.Fatalf("unexpected memberships: %v", got)
	}
}
