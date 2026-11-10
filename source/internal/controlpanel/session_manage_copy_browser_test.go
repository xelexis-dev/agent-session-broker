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

// Opt-in browser evidence uses real handlers/templates and isolated synthetic
// stores. The external capture script never needs real server credentials.
func TestSessionManageCopyFullChrome(t *testing.T) {
	script := os.Getenv("ASB_MANAGE_COPY_BROWSER")
	if script == "" {
		t.Skip("opt-in full Chrome")
	}
	type auth struct {
		Base   string `json:"base"`
		Cookie string `json:"cookie"`
	}
	config := map[string]auth{}
	for _, mode := range []string{"on", "off"} {
		s := userStore(t)
		s.data.ScopeIssuanceReady = policyBool(mode == "on")
		h := New(s, tokenViewBackend{}, http.NotFoundHandler())
		server := httptest.NewServer(h)
		defer server.Close()
		for _, user := range []string{"owner", "admin"} {
			client := loginPanelUser(t, h, user)
			config[mode+"-"+user] = auth{server.URL, client.cookie.Value}
		}
	}
	_, audit := newDirectAuditPanelFixture(t)
	server := httptest.NewServer(audit)
	defer server.Close()
	client := loginPanelUser(t, audit, "owner")
	config["audit-owner"] = auth{server.URL, client.cookie.Value}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal("fixture config failed")
	}
	file := filepath.Join(t.TempDir(), "browser-private.json")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(t.Context(), "node", script, file).CombinedOutput(); err != nil {
		t.Fatalf("full Chrome capture failed: %v\n%s", err, output)
	}
}
