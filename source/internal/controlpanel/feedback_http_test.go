package controlpanel

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSaveFeedbackSurvivesRedirectOnce(t *testing.T) {
	for _, tc := range []struct {
		path, target string
		values       url.Values
	}{
		{"/profile/save", "/profile", url.Values{"displayName": {"Changed"}, "email": {"changed@example.test"}}},
		{"/projects/save", "/projects/settings?project=demo", url.Values{"name": {"demo"}, "mode": {"token"}, "enabled": {"true"}}},
		{"/admin/http-policy", "/admin/settings", url.Values{"allowHTTP": {"true"}}},
		{"/admin/users/update", "/login", url.Values{"id": {"admin"}, "role": {"admin"}, "enabled": {"true"}, "password": {"next test password 220"}, "password_confirm": {"next test password 220"}}},
		{"/admin/users/create", "/admin/users", url.Values{"username": {"new-user"}, "password": {testPassword}, "role": {"user"}}},
		{"/profile/password", "/login", url.Values{"currentPassword": {testPassword}, "newPassword": {"next test password 220"}, "confirmPassword": {"next test password 220"}}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			s := userStore(t)
			h := New(s, &panelBackend{}, http.NotFoundHandler())
			c := loginPanelUser(t, h, "admin")
			saved := c.call("POST", tc.path, tc.values)
			if saved.Code != 303 || saved.Header().Get("Location") != tc.target {
				t.Fatalf("save failed: %d %s", saved.Code, saved.Header().Get("Location"))
			}
			get := func() *httptest.ResponseRecorder {
				req := httptest.NewRequest("GET", "http://broker"+tc.target, nil)
				req.AddCookie(c.cookie)
				for _, cookie := range saved.Result().Cookies() {
					req.AddCookie(cookie)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				return w
			}
			first := get()
			if first.Code != 200 || !strings.Contains(first.Body.String(), `data-toast`) {
				t.Fatal("successful save has no feedback after redirect")
			}
			for _, secret := range []string{testPassword, "next test password 220"} {
				if strings.Contains(first.Body.String(), secret) {
					t.Fatal("feedback exposed password")
				}
			}
			if strings.Contains(get().Body.String(), `data-toast`) {
				t.Fatal("feedback replayed on reload")
			}
		})
	}
}

func TestFailedSaveDoesNotCreateSuccessFeedback(t *testing.T) {
	s := userStore(t)
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	c := loginPanelUser(t, h, "owner")
	bad := c.call("POST", "/projects/save", url.Values{"name": {"demo"}, "mode": {"invalid"}})
	if bad.Code != 400 || strings.Contains(bad.Body.String(), `data-toast`) {
		t.Fatal("failed save shows success")
	}
	for _, cookie := range bad.Result().Cookies() {
		if cookie.Name == "asb_notice" && cookie.MaxAge > 0 {
			t.Fatal("failed save sets success cookie")
		}
	}
	req := httptest.NewRequest("GET", "http://broker/profile", nil)
	req.AddCookie(c.cookie)
	req.AddCookie(&http.Cookie{Name: "asb_notice", Value: "forged"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), `data-toast`) {
		t.Fatal("forged notice accepted")
	}
}
