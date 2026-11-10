package controlpanel

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGenerationHistoryFullChrome(t *testing.T) {
	if os.Getenv("ASB_HISTORY_BROWSER") == "" {
		t.Skip("opt-in full Chrome")
	}
	type browserAuth struct{ Base, CookieName, Cookie string }
	config := map[string]browserAuth{}
	for name, kinds := range map[string][]string{"unknown": {"current", "unknown"}, "previous": {"current", "previous"}, "mixed": {"current", "unknown", "previous"}} {
		_, _, h := generationHistoryFixture(t, kinds)
		server := httptest.NewServer(h)
		defer server.Close()
		c := loginPanelUser(t, h, "admin")
		config[name] = browserAuth{server.URL, c.cookie.Name, c.cookie.Value}
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal("browser config failed")
	}
	private := filepath.Join(t.TempDir(), "browser-private.json")
	if os.WriteFile(private, data, 0600) != nil {
		t.Fatal("browser config write failed")
	}
	cmd := exec.CommandContext(t.Context(), "node", "generation_history_browser.cjs", private)
	if err := cmd.Run(); err != nil {
		t.Fatal("full Chrome assertions failed; inspect sanitized evidence result")
	}
}
