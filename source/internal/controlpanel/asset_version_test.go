package controlpanel

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Browsers kept tokens.css (cached for a day) while panel.css and
// design.css were reloaded, mixing versions. Every stylesheet the pages link is
// addressed by a content version, so an upgrade changes the URL.
var linkedStylesheets = map[string]string{
	"/assets/focus.css": "focus.css",
	"/assets/fonts/pretendard/pretendardvariable-dynamic-subset.css": "fonts/pretendard/pretendardvariable-dynamic-subset.css",
	"/assets/tokens.css":  "tokens.css",
	"/assets/panel.css":   "panel.css",
	"/assets/design.css":  "design.css",
	"/assets/select.css":  "select.css",
	"/assets/switch.css":  "switch.css",
	"/assets/tooltip.css": "tooltip.css",
}

func TestAssetVersionFollowsContent(t *testing.T) {
	sum := sha256.Sum256([]byte("a"))
	if got := assetVersion([]byte("a")); got != hex.EncodeToString(sum[:])[:12] {
		t.Errorf("assetVersion = %q", got)
	}
	if assetVersion([]byte("a")) == assetVersion([]byte("b")) {
		t.Error("different content has the same version")
	}
	if _, err := assetURL("/assets/unknown.css"); err == nil {
		t.Error("an unknown stylesheet got a URL")
	}
}

func TestPagesLinkStylesheetsByContentVersion(t *testing.T) {
	unconfigured, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unconfigured.Close() })
	signedOut := New(userStore(t), &panelBackend{}, http.NotFoundHandler())
	get := func(h http.Handler, path string) string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://broker"+path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, w.Code)
		}
		return w.Body.String()
	}
	bodies := map[string]string{
		"setup": get(New(unconfigured, &panelBackend{}, http.NotFoundHandler()), "/setup"),
		"login": get(signedOut, "/login"),
		"panel": loginPanelUser(t, New(userStore(t), &panelBackend{}, http.NotFoundHandler()), "owner").call("GET", "/", nil).Body.String(),
	}
	for page, body := range bodies {
		for path, file := range linkedStylesheets {
			data, err := assets.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if want := `<link rel="stylesheet" href="` + path + `?v=` + assetVersion(data) + `">`; !strings.Contains(body, want) {
				t.Errorf("%s lacks %s", page, want)
			}
			if strings.Contains(body, `href="`+path+`"`) {
				t.Errorf("%s still links %s without a version", page, path)
			}
		}
	}
}

func TestVersionedStylesheetsAreImmutableAndUnversionedFontsKeepShortCache(t *testing.T) {
	h := New(userStore(t), &panelBackend{}, http.NotFoundHandler())
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://broker"+path, nil))
		return w
	}
	for path, file := range linkedStylesheets {
		data, _ := assets.ReadFile(file)
		w := get(path + "?v=" + assetVersion(data))
		if w.Code != 200 || w.Body.String() != string(data) || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Errorf("%s = %d cache %q", path, w.Code, w.Header().Get("Cache-Control"))
		}
		if old := get(path + "?v=000000000000"); old.Code != 200 || old.Body.String() != string(data) {
			t.Errorf("%s with an old version = %d", path, old.Code)
		}
	}
	for _, path := range []string{"/assets/fonts/pretendard/woff2-dynamic-subset/PretendardVariable.subset.0.woff2", "/assets/icons/lucide-command.svg"} {
		if w := get(path); w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=86400" {
			t.Errorf("%s = %d cache %q", path, w.Code, w.Header().Get("Cache-Control"))
		}
	}
}
