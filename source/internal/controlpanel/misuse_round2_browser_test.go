package controlpanel

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRound2FullChrome(t *testing.T) {
	if os.Getenv("ASB_ROUND2_BROWSER") == "" {
		t.Skip("opt-in full Chrome")
	}
	s, _, h := round2Fixture(t)
	if err := s.SetScopeIssuanceReady("admin", true); err != nil {
		t.Fatal(err)
	}
	seedRound2Audit(t, s, "demo", "previous-owner-private-session")
	owner := loginPanelUser(t, h, "owner")
	if w := owner.call("POST", "/projects/delete", url.Values{"project": {"demo"}, "confirmName": {"demo"}}); w.Code != 303 {
		t.Fatal("fixture deletion failed")
	}
	if err := s.CreateProjectFor("outsider", "demo"); err != nil {
		t.Fatal(err)
	}
	c := loginPanelUser(t, h, "outsider")
	server := httptest.NewServer(h)
	defer server.Close()
	raw, err := json.Marshal(struct{ Base, CookieName, Cookie string }{server.URL, c.cookie.Name, c.cookie.Value})
	if err != nil {
		t.Fatal("browser config failed")
	}
	private := filepath.Join(t.TempDir(), "browser-private.json")
	if err := os.WriteFile(private, raw, 0600); err != nil {
		t.Fatal("browser config write failed")
	}
	if err := exec.CommandContext(t.Context(), "node", "misuse_round2_browser.cjs", private).Run(); err != nil {
		t.Fatal("full Chrome assertions failed; inspect sanitized evidence")
	}
}
