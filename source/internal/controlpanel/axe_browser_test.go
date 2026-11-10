package controlpanel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func TestPanelAxeFullChrome(t *testing.T) {
	if os.Getenv("ASB_AXE_BROWSER") == "" {
		t.Skip("opt-in full Chrome")
	}
	s := userStore(t)
	if err := s.SetScopeIssuanceReady("admin", true); err != nil {
		t.Fatal(err)
	}
	raw, err := s.IssueProjectTokenWithOptionsFor("admin", "demo", "axe-fixture", ProjectTokenOptions{Scopes: []string{access.SessionRegister, access.SessionRead}})
	if err != nil {
		t.Fatal("fixture token failed")
	}
	parent := scopeEditParent(t, s, raw)
	device, err := s.BeginDeviceAuthorization("demo", raw, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", InstanceID: "axe-pc", SessionID: "axe-session", RequestID: "axe-request"})
	if err != nil {
		t.Fatal("fixture device failed")
	}
	server := httptest.NewServer(New(s, tokenViewBackend{sessions: []Session{{ID: "axe-session", Name: "axe-worker", Harness: "codex", Status: "alive", OwnerID: "admin"}}}, http.NotFoundHandler()))
	defer server.Close()
	setupStore, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer setupStore.Close()
	setup := httptest.NewServer(New(setupStore, tokenViewBackend{}, http.NotFoundHandler()))
	defer setup.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/users/detail" {
			http.Redirect(w, r, "/admin/users", http.StatusFound)
			return
		}
		server.Config.Handler.ServeHTTP(w, r)
	}))
	defer redirect.Close()
	for _, tc := range []struct {
		name, base string
		denied     bool
	}{{"normal", server.URL, false}, {"redirect", redirect.URL, true}} {
		t.Run(tc.name, func(t *testing.T) {
			config, err := json.Marshal(map[string]string{"base": tc.base, "setup": setup.URL, "password": testPassword, "parent": parent.ID, "device": device.UserCode})
			if err != nil {
				t.Fatal("fixture config failed")
			}
			file := filepath.Join(t.TempDir(), "axe-private.json")
			if err := os.WriteFile(file, config, 0600); err != nil {
				t.Fatal("fixture write failed")
			}
			evidence := os.Getenv("ASB_AXE_EVIDENCE")
			if tc.denied {
				evidence = filepath.Join(evidence, tc.name)
			}
			cmd := exec.CommandContext(t.Context(), "node", "axe_browser.cjs", file)
			cmd.Env = append(os.Environ(), "ASB_AXE_EVIDENCE="+evidence)
			err = cmd.Run()
			if !tc.denied {
				if err != nil {
					t.Fatal("axe browser assertions failed; inspect sanitized axe evidence")
				}
				return
			}
			if err == nil {
				t.Fatal("user-detail redirect was accepted as screen coverage")
			}
			data, err := os.ReadFile(filepath.Join(evidence, "failure.json"))
			var failure struct{ Stage string }
			if err != nil || json.Unmarshal(data, &failure) != nil || failure.Stage != "user-detail-light" {
				t.Fatal("redirect must fail at user-detail identity check")
			}
			if err := os.Rename(filepath.Join(evidence, "failure.json"), filepath.Join(evidence, "expected-redirect.json")); err != nil {
				t.Fatal("redirect evidence failed")
			}
		})
	}
}
