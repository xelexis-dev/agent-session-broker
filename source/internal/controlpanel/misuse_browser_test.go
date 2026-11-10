package controlpanel

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Opt-in real Chrome check. Private login data is never printed or captured.
func TestMisuseFullChrome(t *testing.T) {
	if os.Getenv("ASB_MISUSE_BROWSER") == "" {
		t.Skip("opt-in full Chrome")
	}
	_, _, h := misuseFixture(t)
	server := httptest.NewServer(h)
	defer server.Close()
	c := loginPanelUser(t, h, "owner")
	config, err := json.Marshal(struct{ Base, CookieName, Cookie string }{server.URL, c.cookie.Name, c.cookie.Value})
	if err != nil {
		t.Fatal("browser config encoding failed")
	}
	private := filepath.Join(t.TempDir(), "browser-private.json")
	if os.WriteFile(private, config, 0600) != nil {
		t.Fatal("browser config write failed")
	}
	cmd := exec.CommandContext(t.Context(), "node", "misuse_browser.cjs", private)
	if err := cmd.Run(); err != nil {
		t.Fatal("full Chrome assertions failed; inspect sanitized browser evidence")
	}
}
