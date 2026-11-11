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

// Full Chrome: the translated shell and pre-login pages in English and Korean (see shell_language_browser.cjs).
func TestShellLanguageBrowserFullChrome(t *testing.T) {
	if os.Getenv("ASB_SHELL_LANGUAGE_BROWSER") == "" {
		t.Skip("opt-in shell language full Chrome")
	}
	s := userStore(t)
	if err := s.UpdateLanguage("owner", "ko"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(s, &panelBackend{}, http.NotFoundHandler()))
	defer server.Close()
	data, _ := json.Marshal(map[string]string{"base": server.URL, "password": testPassword})
	config := filepath.Join(t.TempDir(), "shell-language-browser.json")
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "node", "shell_language_browser.cjs", config)
	if err := cmd.Run(); err != nil {
		t.Fatal("shell language browser/axe check failed; inspect sanitized evidence")
	}
}
