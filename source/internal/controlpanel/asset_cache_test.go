package controlpanel

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestVersionedScriptIsImmutable(t *testing.T) {
	data, err := assets.ReadFile("panel.js")
	if err != nil {
		t.Fatal(err)
	}
	h := New(userStore(t), &panelBackend{}, http.NotFoundHandler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://broker/assets/panel.js?v="+assetVersion(data), nil))
	if w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("versioned JS: status=%d cache=%q", w.Code, w.Header().Get("Cache-Control"))
	}
	if !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("asset body changed")
	}
}

func TestPagesVersionEveryScript(t *testing.T) {
	h := New(userStore(t), &panelBackend{}, http.NotFoundHandler())
	c := loginPanelUser(t, h, "owner")
	scripts := regexp.MustCompile(`<script[^>]*src="([^"]+)"`)
	for _, path := range []string{"/login", "/", "/projects/sessions?project=demo"} {
		w := c.call("GET", path, nil)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("HTML cache changed: %s", path)
		}
		matches := scripts.FindAllStringSubmatch(w.Body.String(), -1)
		if len(matches) < 6 {
			t.Fatal("missing scripts")
		}
		for _, match := range matches {
			url := match[1]
			file := strings.TrimPrefix(strings.Split(url, "?")[0], "/assets/")
			data, err := assets.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if want := "/assets/" + file + "?v=" + assetVersion(data); url != want {
				t.Errorf("%s script %s want%s", path, url, want)
			}
		}
	}
}

func TestAssetCacheUpgradeRollbackAndInvalidVersions(t *testing.T) {
	h := New(userStore(t), &panelBackend{}, http.NotFoundHandler())
	files := map[string]string{}
	for path := range panelFiles {
		files[path] = strings.TrimPrefix(path, "/assets/")
	}
	for path, asset := range designAssets {
		files[path] = asset.File
	}
	for path, file := range files {
		data, err := assets.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		current := assetVersion(data)
		// Different bytes from an older release or a release being rolled back must
		// never be pinned as this release's content, even when its path is unchanged.
		old := assetVersion(append(bytes.Clone(data), []byte("previous-release")...))
		future := assetVersion(append(bytes.Clone(data), []byte("next-release")...))
		for _, query := range []string{"?v=" + current, "", "?v=wrong", "?v=", "?v=" + old, "?v=" + future, "?v=" + current + "&v=" + current, "?v=" + current + "&v=wrong"} {
			for _, method := range []string{"GET", "HEAD"} {
				req := httptest.NewRequest(method, "http://broker"+path+query, nil)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				immutable := query == "?v="+current
				if w.Code != 200 || strings.Contains(w.Header().Get("Cache-Control"), "immutable") != immutable {
					t.Errorf("%s %s%s status=%d cache=%q", method, path, query, w.Code, w.Header().Get("Cache-Control"))
				}
				if method == "HEAD" {
					if w.Body.Len() != 0 {
						t.Fatal("HEAD has body")
					}
				} else if !bytes.Equal(w.Body.Bytes(), data) {
					t.Errorf("%s body changed", path)
				}
			}
		}
	}
}
