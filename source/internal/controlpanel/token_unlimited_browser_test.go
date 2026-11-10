package controlpanel

import (
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Opt-in real browser, synthetic data, no shared server or credentials.
func TestAutoTokenUnlimitedFullChrome(t *testing.T) {
	script := os.Getenv("ASB_UNLIMITED_BROWSER")
	if script == "" {
		t.Skip("opt-in full Chrome")
	}
	s := userStore(t)
	server := httptest.NewServer(New(s, tokenViewBackend{}, http.NotFoundHandler()))
	defer server.Close()
	settings := map[string]string{"base": server.URL, "username": "admin", "password": testPassword}
	file := filepath.Join(t.TempDir(), "browser.json")
	data, _ := json.Marshal(settings)
	if os.WriteFile(file, data, 0600) != nil {
		t.Fatal("browser config failed")
	}
	cmd := exec.CommandContext(t.Context(), "node", script, file)
	if _, err := cmd.CombinedOutput(); err != nil {
		t.Fatal("full Chrome failed (output withheld)")
	}
	tokens := s.ProjectsFor("owner")[0].Tokens
	if len(tokens) != 1 || !access.HasScope(tokens[0].Scopes, access.SessionRegisterAuto) || tokens[0].MaxAutoBindings != 0 || !tokens[0].ExpiresAt.IsZero() {
		t.Fatal("browser did not issue unlimited token")
	}
}
