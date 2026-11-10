package controlpanel

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const pretendardCSS = "/assets/fonts/pretendard/pretendardvariable-dynamic-subset.css"

// The UI font is the pretendard@1.3.9 npm package, copied byte for byte.
// Pretendard is the only font family shipped for all panel text.
func TestPretendardIsTheEmbeddedUIFont(t *testing.T) {
	woff2 := 0
	for p, asset := range designAssets {
		if !strings.HasPrefix(p, "/assets/fonts/") {
			continue
		}
		switch {
		case strings.HasPrefix(p, "/assets/fonts/pretendard/"):
			source, _ := asset.Source["package"].(string)
			if source != "pretendard" || asset.Source["version"] != "1.3.9" || asset.License != "OFL-1.1" {
				t.Errorf("%s does not come from pretendard@1.3.9 under OFL-1.1: %+v", p, asset)
			}
			if strings.HasSuffix(p, ".woff2") {
				woff2++
			}
		default:
			t.Errorf("unexpected font asset %s", p)
		}
	}
	if woff2 != 92 {
		t.Errorf("manifest lists %d Pretendard subsets, want 92", woff2)
	}
	for _, p := range []string{pretendardCSS, "/assets/fonts/pretendard/LICENSE.txt"} {
		if _, ok := designAssets[p]; !ok {
			t.Errorf("%s is not listed", p)
		}
	}
	err := fs.WalkDir(assets, "fonts", func(name string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasPrefix(name, "fonts/pretendard/") {
			t.Errorf("%s is embedded", name)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPretendardStylesheetOnlyReferencesListedFiles(t *testing.T) {
	css, err := assets.ReadFile(designAssets[pretendardCSS].File)
	if err != nil {
		t.Fatal(err)
	}
	urls := regexp.MustCompile(`url\(([^)]+)\)`).FindAllStringSubmatch(string(css), -1)
	if len(urls) != 92 {
		t.Fatalf("stylesheet has %d font URLs, want 92", len(urls))
	}
	for _, m := range urls {
		target := path.Join(path.Dir(pretendardCSS), strings.Trim(m[1], `"'`))
		if asset, ok := designAssets[target]; !ok || asset.ContentType != "font/woff2" {
			t.Errorf("%s resolves to unlisted %s", m[1], target)
		}
	}
	tokens, _ := assets.ReadFile("tokens.css")
	if !strings.Contains(string(tokens), `--font-latin: var(--font-sans);`) {
		t.Error("Latin alias does not resolve to Pretendard")
	}
	if !strings.Contains(string(tokens), `--font-sans: "Pretendard Variable", `) {
		t.Error("tokens.css does not make Pretendard the sans font")
	}
}

func TestPagesLoadThePretendardStylesheet(t *testing.T) {
	versioned, err := assetURL(pretendardCSS)
	if err != nil {
		t.Fatal(err)
	}
	link := `<link rel="stylesheet" href="` + versioned + `">`
	h := New(userStore(t), &panelBackend{}, http.NotFoundHandler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://broker/login", nil))
	if !strings.Contains(w.Body.String(), link) {
		t.Error("login page does not load Pretendard")
	}
	if body := loginPanelUser(t, h, "owner").call("GET", "/", nil).Body.String(); !strings.Contains(body, link) {
		t.Error("panel page does not load Pretendard")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://broker/assets/fonts/pretendard/woff2-dynamic-subset/PretendardVariable.subset.0.woff2", nil))
	if w.Code != 200 || w.Header().Get("Content-Type") != "font/woff2" {
		t.Errorf("Pretendard subset = %d %q", w.Code, w.Header().Get("Content-Type"))
	}
}

func TestPanelCSSAndEmbeddedFontsArePretendardOnly(t *testing.T) {
	handler := New(userStore(t), &panelBackend{}, http.NotFoundHandler())
	cssPaths := map[string]bool{}
	for p, ct := range panelFiles {
		if strings.HasPrefix(ct, "text/css") {
			cssPaths[p] = true
		}
	}
	for p, a := range designAssets {
		if strings.HasPrefix(a.ContentType, "text/css") {
			cssPaths[p] = true
		}
	}
	for p := range cssPaths {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "http://broker"+p, nil))
		if w.Code != http.StatusOK {
			t.Errorf("CSS %s not served", p)
			continue
		}
		for _, declaration := range regexp.MustCompile(`font-family\s*:\s*([^;}]+)`).FindAllStringSubmatch(w.Body.String(), -1) {
			family := strings.TrimSpace(declaration[1])
			if family != "var(--font-sans)" && family != "var(--font-latin)" && strings.Trim(family, `"'`) != "Pretendard Variable" {
				t.Errorf("CSS %s declares an unexpected font family: %s", p, family)
			}
		}
		text := strings.ToLower(w.Body.String())
		for _, forbidden := range []string{"roboto mono", "monospace", "menlo", "--font-mono"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("served CSS %s still names %s", p, forbidden)
			}
		}
	}
	if err := fs.WalkDir(assets, ".", func(name string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.Contains(name, "roboto-mono") {
			t.Errorf("legacy font asset embedded: %s", name)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyPretendardFontsRemainOnDisk(t *testing.T) {
	if err := filepath.WalkDir("fonts", func(name string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasPrefix(filepath.ToSlash(name), "fonts/pretendard/") {
			t.Errorf("unexpected font file on disk: %s", name)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("fonts.css"); !os.IsNotExist(err) {
		t.Fatal("obsolete font stylesheet remains on disk")
	}
}
