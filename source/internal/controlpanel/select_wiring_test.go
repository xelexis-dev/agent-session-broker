package controlpanel

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The shared select is served, loaded as a module, and opted into by
// the four role selects (search stays off for these short lists).
func TestSharedSelectIsServedAndUsedByTheRoleSelects(t *testing.T) {
	h := New(userStore(t), &panelBackend{}, http.NotFoundHandler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://broker/assets/select.js", nil))
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") || !strings.Contains(w.Body.String(), "export function initSelects") {
		t.Fatalf("select.js = %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	admin := loginPanelUser(t, h, "admin")
	owner := loginPanelUser(t, h, "owner")
	selects := regexp.MustCompile(`<select name="role"[^>]*>`)
	for _, tc := range []struct {
		name  string
		body  string
		count int
	}{
		{"project members", owner.call("GET", "/projects/settings?project=demo", nil).Body.String(), 3}, // two member rows + add member
		{"user create", admin.call("GET", "/admin/users", nil).Body.String(), 1},
		{"user detail", admin.call("GET", "/admin/users/detail?id=owner", nil).Body.String(), 1},
	} {
		found := selects.FindAllString(tc.body, -1)
		if len(found) < tc.count {
			t.Fatalf("%s: %d role selects, want at least %d", tc.name, len(found), tc.count)
		}
		for _, tag := range found {
			if !strings.Contains(tag, " data-select") || strings.Contains(tag, "data-select-search") {
				t.Errorf("%s: %s", tc.name, tag)
			}
		}
		url, err := assetURL("/assets/select.js")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(tc.body, `<script type="module" src="`+url+`"></script>`) {
			t.Errorf("%s does not load select.js", tc.name)
		}
	}
}

func TestEveryPanelSelectUsesSharedComponent(t *testing.T) {
	files, err := filepath.Glob("*.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, tag := range regexp.MustCompile(`<select\b[^>]*>`).FindAllString(string(data), -1) {
			if !strings.Contains(tag, " data-select") {
				t.Errorf("%s has a native-only select", file)
			}
		}
	}
}
