package controlpanel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The login and setup pages follow the revised Figma frames, which the
// Storybook catalog shares: dotted password placeholders, the short tablet
// tagline next to the full one, and the panel label kept for tablet widths.
func TestAccessPagesMatchTheRevisedLoginDesign(t *testing.T) {
	unconfigured, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unconfigured.Close() })
	const dots = `placeholder="••••••••••••"`
	for path, store := range map[string]*Store{"/setup": unconfigured, "/login": userStore(t)} {
		h := New(store, &panelBackend{}, http.NotFoundHandler())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://broker"+path, nil))
		body := w.Body.String()
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, w.Code)
		}
		passwords := strings.Count(body, `type="password"`)
		if passwords == 0 || strings.Count(body, dots) != passwords {
			t.Errorf("%s: %d password inputs, %d with the dotted placeholder", path, passwords, strings.Count(body, dots))
		}
		for _, want := range []string{
			`<span class="login__lead-long">` + en("access.lead") + `</span>`,
			`<span class="login__lead-short">` + en("access.lead") + `</span>`,
			`<span class="mono-label login__panel-label">ASB Control Panel</span>`,
			`name="csrf"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %s", path, want)
			}
		}
		if strings.Contains(body, "data-password-toggle") {
			t.Errorf("%s has a password visibility button", path)
		}
	}
}
