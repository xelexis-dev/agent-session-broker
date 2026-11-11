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

func TestLanguageBrowserFullChrome(t *testing.T) {
	if os.Getenv("ASB_LANGUAGE_BROWSER") == "" {
		t.Skip("opt-in language selector full Chrome")
	}
	s := userStore(t)
	if _, err := s.IssueProjectTokenFor("admin", "demo", "catalog-parent"); err != nil {
		t.Fatal("create synthetic project token")
	}
	editor := ""
	for _, project := range s.ProjectsFor("admin") {
		if project.Name == "demo" {
			for _, token := range project.Tokens {
				if token.Label == "catalog-parent" {
					editor = token.ID
				}
			}
		}
	}

	server := httptest.NewServer(New(s, &panelBackend{}, http.NotFoundHandler()))
	defer server.Close()
	data, _ := json.Marshal(map[string]string{"base": server.URL, "password": testPassword, "editor": editor})
	config := filepath.Join(t.TempDir(), "language-browser.json")
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "node", "language_browser.cjs", config)
	if err := cmd.Run(); err != nil {
		t.Fatal("language browser/axe check failed; inspect sanitized evidence")
	}
}
