package controlpanel

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestSetupPasswordConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, confirmation string
		want               int
	}{
		{"missing", "", http.StatusBadRequest},
		{"mismatch", "different confirmation input", http.StatusBadRequest},
		{"matching", testPassword, http.StatusSeeOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := OpenSQLite(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			h := New(s, &panelBackend{}, http.NotFoundHandler())
			form := httptest.NewRecorder()
			h.ServeHTTP(form, httptest.NewRequest("GET", "http://broker/setup", nil))
			csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(form.Body.String())
			if len(csrf) != 2 {
				t.Fatal("setup form missing CSRF")
			}
			confirmation := regexp.MustCompile(`<input[^>]*name="confirmPassword"[^>]*>`).FindString(form.Body.String())
			for _, attr := range []string{`type="password"`, `required`, `autocomplete="new-password"`} {
				if !strings.Contains(confirmation, attr) {
					t.Errorf("confirmation input missing %s", attr)
				}
			}
			values := url.Values{"setupCode": {setupCodeForTest(t, s)}, "csrf": {csrf[1]}, "password": {testPassword}}
			if tc.confirmation != "" {
				values.Set("confirmPassword", tc.confirmation)
			}
			post := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest("POST", "http://broker/setup", strings.NewReader(values.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				r.AddCookie(form.Result().Cookies()[0])
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}
			response := post()
			if response.Code != tc.want {
				t.Errorf("setup = %d, want %d", response.Code, tc.want)
			}
			if strings.Contains(response.Body.String(), testPassword) || (tc.confirmation != "" && strings.Contains(response.Body.String(), tc.confirmation)) {
				t.Fatal("setup response exposed password input")
			}
			if tc.want == http.StatusBadRequest {
				if s.Ready() {
					t.Fatal("rejected confirmation created administrator")
				}
				if _, exists := s.User("admin"); exists {
					t.Fatal("rejected confirmation created admin record")
				}
				return
			}
			if !s.Ready() || !s.Authenticate(testPassword) {
				t.Fatal("matching confirmation did not configure admin")
			}
			if again := post(); again.Code != http.StatusConflict {
				t.Fatalf("repeat setup = %d", again.Code)
			}
			login := httptest.NewRecorder()
			h.ServeHTTP(login, httptest.NewRequest("GET", "http://broker/login", nil))
			if strings.Contains(login.Body.String(), `name="confirmPassword"`) {
				t.Fatal("login asks for password confirmation")
			}
			loginPanelUser(t, h, "admin")
		})
	}
}
