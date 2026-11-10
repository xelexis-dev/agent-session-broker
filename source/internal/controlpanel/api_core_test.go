package controlpanel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPILoginLifecycle(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAdminAPI("admin", true); err != nil {
		t.Fatal(err)
	}
	h := New(s, nil, http.NotFoundHandler())
	r := httptest.NewRequest("POST", "https://example.test/api/v1/login", strings.NewReader(`{"username":"admin","password":"`+testPassword+`"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatalf("login status=%d, want 201", w.Code)
	}
	var token struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(w.Body.Bytes(), &token) != nil || token.Token == "" {
		t.Fatal("missing one-time token")
	}
	for _, step := range []struct {
		method, path string
		status       int
	}{{"GET", "/api/v1/me", 200}, {"GET", "/api/v1/me/tokens", 200}, {"POST", "/api/v1/logout", 204}, {"GET", "/api/v1/me", 401}} {
		r = httptest.NewRequest(step.method, "https://example.test"+step.path, nil)
		r.Header.Set("Authorization", "Bearer "+token.Token)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != step.status {
			t.Fatalf("%s status=%d want=%d", step.path, w.Code, step.status)
		}
	}
	r = httptest.NewRequest("GET", "https://example.test/api/v1/me", nil)
	r.AddCookie(&http.Cookie{Name: "asb_admin", Value: "synthetic"})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("cookie-only=%d", w.Code)
	}
}
