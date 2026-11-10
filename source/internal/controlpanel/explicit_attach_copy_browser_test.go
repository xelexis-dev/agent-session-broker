package controlpanel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Opt-in full Chrome capture of the real templates and assets with synthetic
// data. This never changes a shared server or issues a real credential.
func TestExplicitAttachCopyFullChrome(t *testing.T) {
	script := os.Getenv("ASB_EXPLICIT_COPY_BROWSER")
	if script == "" {
		t.Skip("opt-in full Chrome")
	}
	s := userStore(t)
	assets := New(s, tokenViewBackend{}, http.NotFoundHandler())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/capture/explicit-attach" {
			v := uiCopyPreview("settings")
			v.AutoSelected = true
			render(w, http.StatusOK, v)
			return
		}
		assets.ServeHTTP(w, r)
	}))
	defer server.Close()
	data, err := json.Marshal(map[string]string{"base": server.URL})
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "browser.json")
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "node", script, config)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("full Chrome capture failed: %v\n%s", err, output)
	}
}
