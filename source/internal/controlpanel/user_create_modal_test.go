package controlpanel

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestUserCreateModalInitialAndSuccessfulCreate(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	admin := loginPanelUser(t, New(s, &panelBackend{}, http.NotFoundHandler()), "admin")
	w := admin.call("GET", "/admin/users", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("users page = %d", w.Code)
	}
	body := w.Body.String()
	if !regexp.MustCompile(`(?s)<header\b[^>]*>.*data-dialog-open="create-user".*</header>`).MatchString(body) {
		t.Error("page header cannot open the user creation dialog")
	}
	dialog := regexp.MustCompile(`(?s)<dialog\b[^>]*id="create-user"[^>]*>.*?</dialog>`).FindString(body)
	if dialog == "" {
		t.Fatal("user creation dialog missing")
	}
	if strings.Contains(dialog, "data-dialog-reopen") || regexp.MustCompile(`<dialog[^>]*\sopen(?:\s|=|>)`).MatchString(dialog) {
		t.Error("initial dialog should be closed")
	}
	for _, want := range []string{`aria-labelledby="create-user-title"`, `id="create-user-title"`, `action="/admin/users/create"`, `name="csrf" value="` + admin.csrf + `"`, `data-dialog-close`, `name="username"`, `name="password"`, `name="role"`} {
		if !strings.Contains(dialog, want) {
			t.Errorf("dialog missing %s", want)
		}
	}
	if strings.Count(body, `action="/admin/users/create"`) != 1 {
		t.Error("creation form must appear only inside the dialog")
	}
	w = admin.call("POST", "/admin/users/create", url.Values{"username": {"new-admin"}, "password": {testPassword}, "role": {"admin"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin/users" {
		t.Fatalf("create redirect = %d %s", w.Code, w.Header().Get("Location"))
	}
	if u, ok := s.AuthenticateUser("new-admin", testPassword); !ok || u.Role != "admin" {
		t.Error("created user cannot authenticate with the submitted role")
	}
	w = admin.call("GET", "/admin/users", nil)
	if strings.Contains(w.Body.String(), `data-dialog-reopen="create-user"`) {
		t.Error("successful create reopened the dialog")
	}
}

func TestUserCreateModalFailurePreservesSafeFields(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	admin := loginPanelUser(t, New(s, &panelBackend{}, http.NotFoundHandler()), "admin")
	for _, tc := range []struct {
		name, username, password, role string
	}{
		{"duplicate", "admin", testPassword, "admin"},
		{"short password", "new-user", "too-short", "user"},
		{"invalid username", "Bad Name", testPassword, "admin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := admin.call("POST", "/admin/users/create", url.Values{"username": {tc.username}, "password": {tc.password}, "role": {tc.role}})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("invalid create = %d", w.Code)
			}
			body := w.Body.String()
			dialog := regexp.MustCompile(`(?s)<dialog\b[^>]*id="create-user"[^>]*>.*?</dialog>`).FindString(body)
			if !strings.Contains(dialog, `data-dialog-reopen="create-user"`) {
				t.Error("failed create did not reopen its dialog")
			}
			if !strings.Contains(dialog, `name="username" value="`+tc.username+`"`) {
				t.Error("failed create lost the username")
			}
			if !regexp.MustCompile(`<option value="` + tc.role + `"\s+selected(?:\s|>)`).MatchString(dialog) {
				t.Error("failed create lost the role selection")
			}
			if !strings.Contains(dialog, `role="alert"`) || strings.Count(body, `role="alert"`) != 1 {
				t.Error("creation error must appear once inside the dialog")
			}
			if strings.Contains(body, tc.password) || regexp.MustCompile(`<input[^>]*type="password"[^>]*\svalue=`).MatchString(body) {
				t.Error("submitted password was rendered back to the page")
			}
		})
	}
	w := admin.call("POST", "/admin/users/update", url.Values{"id": {"admin"}, "role": {"user"}})
	if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), `data-dialog-reopen="create-user"`) || strings.Count(w.Body.String(), `role="alert"`) != 1 {
		t.Error("user update error must remain on the page without reopening creation")
	}
}

func TestUserCreateModalRetainsCSRFAndAdminChecks(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("admin", "ordinary", testPassword, "user"); err != nil {
		t.Fatal(err)
	}
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	admin := loginPanelUser(t, h, "admin")
	admin.csrf = "invalid"
	ordinary := loginPanelUser(t, h, "ordinary")
	for _, client := range []*userPanelClient{admin, ordinary} {
		w := client.call("POST", "/admin/users/create", url.Values{"username": {"forbidden-user"}, "password": {testPassword}, "role": {"admin"}})
		if w.Code != http.StatusForbidden {
			t.Errorf("unauthorized create = %d", w.Code)
		}
	}
	if _, ok := s.User("forbidden-user"); ok {
		t.Error("forbidden request created a user")
	}
	if w := ordinary.call("GET", "/admin/users", nil); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), `id="create-user"`) {
		t.Error("ordinary user can access user creation dialog")
	}
}
