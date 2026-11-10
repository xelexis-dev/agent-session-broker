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

func TestScopeHintFullChrome(t *testing.T) {
	script := os.Getenv("ASB_SCOPE_HINT_BROWSER")
	if script == "" {
		t.Skip("opt-in full Chrome")
	}
	s := userStore(t)
	server := httptest.NewServer(New(s, tokenViewBackend{}, http.NotFoundHandler()))
	defer server.Close()
	config, err := json.Marshal(map[string]string{"base": server.URL, "password": testPassword})
	if err != nil {
		t.Fatal("browser config failed")
	}
	file := filepath.Join(t.TempDir(), "browser.json")
	if err := os.WriteFile(file, config, 0600); err != nil {
		t.Fatal("browser config write failed")
	}
	cmd := exec.CommandContext(t.Context(), "node", script, file)
	if _, err := cmd.CombinedOutput(); err != nil {
		t.Fatal("scope hint full Chrome check failed; output withheld")
	}
}
