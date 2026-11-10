package controlpanel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Static files. The panel's own CSS and scripts are listed here; fonts, icons,
// tokens and their licenses are listed by asset-manifest.json (with sha256
// checked in tests). Nothing outside these two lists is served.
var panelFiles = map[string]string{
	"/assets/session-live.js": "text/javascript; charset=utf-8",
	"/assets/login-return.js": "text/javascript; charset=utf-8",
	"/assets/focus.css":       "text/css; charset=utf-8",
	"/assets/tooltip.css":     "text/css; charset=utf-8",
	"/assets/tooltip.js":      "text/javascript; charset=utf-8",
	"/assets/panel.css":       "text/css; charset=utf-8",
	"/assets/panel.js":        "text/javascript; charset=utf-8",
	"/assets/design.css":      "text/css; charset=utf-8",
	"/assets/theme.js":        "text/javascript; charset=utf-8",
	"/assets/shell.js":        "text/javascript; charset=utf-8",
	"/assets/select.css":      "text/css; charset=utf-8",
	"/assets/select.js":       "text/javascript; charset=utf-8",
	"/assets/switch.css":      "text/css; charset=utf-8",
}

type manifestAsset struct {
	Path        string         `json:"path"`
	File        string         `json:"file"`
	ContentType string         `json:"contentType"`
	SHA256      string         `json:"sha256"`
	Bytes       int64          `json:"bytes"`
	License     string         `json:"license"`
	Source      map[string]any `json:"source"`
}

var designAssets = loadDesignAssets()

func loadDesignAssets() map[string]manifestAsset {
	data, err := assets.ReadFile("asset-manifest.json")
	if err != nil {
		panic(err)
	}
	var manifest struct {
		Assets []manifestAsset `json:"assets"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		panic(err)
	}
	byPath := make(map[string]manifestAsset, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		byPath[asset.Path] = asset
	}
	return byPath
}

// Content versions change with the embedded bytes, so upgrades and rollbacks
// select the matching browser cache entry for each asset.
var assetVersions = loadAssetVersions()

func loadAssetVersions() map[string]string {
	files := make(map[string]string, len(panelFiles)+len(designAssets))
	for path := range panelFiles {
		files[path] = strings.TrimPrefix(path, "/assets/")
	}
	for path, asset := range designAssets {
		files[path] = asset.File
	}
	versions := make(map[string]string, len(files))
	for path, file := range files {
		data, err := assets.ReadFile(file)
		if err != nil {
			panic(err)
		}
		versions[path] = assetVersion(data)
	}
	return versions
}

func assetVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}

// assetURL backs {{asset "..."}} in templates. An unknown path fails the render
// with an error rather than panicking the request.
func assetURL(path string) (string, error) {
	version, ok := assetVersions[path]
	if !ok {
		return "", fmt.Errorf("no versioned asset %s", path)
	}
	return path + "?v=" + version, nil
}

// serveAsset answers GET/HEAD for a listed path and reports whether it did.
func serveAsset(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	file, contentType := "", ""
	if ct, ok := panelFiles[r.URL.Path]; ok {
		file, contentType = r.URL.Path[len("/assets/"):], ct
	} else if asset, ok := designAssets[r.URL.Path]; ok {
		file, contentType = asset.File, asset.ContentType
		// Unversioned fonts and icons retain their existing short cache policy.
		if !strings.HasPrefix(contentType, "text/css") {
			w.Header().Set("Cache-Control", "public, max-age=86400")
		}
	} else {
		return false
	}
	data, err := assets.ReadFile(file)
	if err != nil {
		return false
	}
	// A stale URL may reach this release after an upgrade or rollback. Serving
	// current bytes under that URL must not pin them in the immutable cache.
	if versions := r.URL.Query()["v"]; len(versions) == 1 && versions[0] == assetVersions[r.URL.Path] {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	w.Header().Set("Content-Type", contentType)
	if strings.HasPrefix(contentType, "text/css") || strings.HasPrefix(contentType, "text/javascript") {
		writePanelText(w, r, http.StatusOK, data)
		return true
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return true
	}
	_, _ = w.Write(data)
	return true
}
