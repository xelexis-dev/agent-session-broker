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

func TestProfileRolesFullChrome(t *testing.T) {
	script := os.Getenv("ASB_PROFILE_ROLES_BROWSER")
	if script == "" {
		t.Skip("opt-in full Chrome")
	}
	s := userStore(t)
	for _, item := range []struct{ name, role string }{{"development", "developer"}, {"project-with-a-long-readable-name-that-wraps-in-mobile-profile", "viewer"}} {
		if err := s.CreateProjectFor("admin", item.name); err != nil {
			t.Fatal(err)
		}
		if err := s.SetProjectMember("admin", item.name, "owner", item.role); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(New(s, &panelBackend{}, http.NotFoundHandler()))
	defer server.Close()
	data, err := json.Marshal(map[string]string{"base": server.URL, "password": testPassword})
	if err != nil {
		t.Fatal("browser config failed")
	}
	file := filepath.Join(t.TempDir(), "browser.json")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal("browser config write failed")
	}
	if _, err := exec.CommandContext(t.Context(), "node", script, file).CombinedOutput(); err != nil {
		t.Fatal("profile browser failed; output withheld")
	}
}
