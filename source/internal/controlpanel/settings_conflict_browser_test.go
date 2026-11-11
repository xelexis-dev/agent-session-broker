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

func TestSettingsConflictFullChrome(t *testing.T) {
	if os.Getenv("ASB_SETTINGS_BROWSER") == "" {
		t.Skip("opt-in full Chrome")
	}
	for _, tc := range settingsConflictCases() {
		for _, theme := range []string{"light", "dark"} {
			t.Run(tc.name+"/"+theme, func(t *testing.T) {
				s := userStore(t)
				if err := s.CreateUser("admin", "admin2", testPassword, "admin"); err != nil {
					t.Fatal(err)
				}
				useKorean(t, s)
				h := New(s, &panelBackend{}, http.NotFoundHandler())
				a := loginPanelUser(t, h, "admin")
				actorB := "admin2"
				if tc.name == "profile" {
					actorB = "admin"
				}
				b := loginPanelUser(t, h, actorB)
				server := httptest.NewServer(h)
				defer server.Close()
				first := map[string]string{}
				for k, v := range tc.first {
					first[k] = v[0]
				}
				config := struct {
					Base, Name, Theme, Page, Action, CookieName, CookieA, CookieB, Message string
					First                                                                  map[string]string
				}{server.URL, tc.name, theme, tc.page, tc.action, a.cookie.Name, a.cookie.Value, b.cookie.Value, ko(settingsConflictMessage), first}
				raw, err := json.Marshal(config)
				if err != nil {
					t.Fatal("browser config failed")
				}
				private := filepath.Join(t.TempDir(), "browser-private.json")
				if err = os.WriteFile(private, raw, 0600); err != nil {
					t.Fatal("browser config write failed")
				}
				if err = exec.CommandContext(t.Context(), "node", "settings_conflict_browser.cjs", private).Run(); err != nil {
					t.Fatal("full Chrome assertions failed; inspect sanitized evidence")
				}
			})
		}
	}
}
