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

func TestPretendardFullChrome(t *testing.T) {
	script := os.Getenv("ASB_PRETENDARD_BROWSER")
	if script == "" {
		t.Skip("opt-in full Chrome")
	}
	s := userStore(t)
	if err := s.SetScopeIssuanceReady("admin", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IssueProjectTokenFor("owner", "demo", "synthetic-token-label"); err != nil {
		t.Fatal("fixture token failed")
	}
	handler := New(s, sessionPIDBrowserBackend{}, http.NotFoundHandler())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/capture/font-issued" {
			v := uiCopyPreview("settings")
			v.IssuedToken = "SYNTHETIC-PREVIEW-NOT-A-CREDENTIAL"
			render(w, 200, v)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	config, err := json.Marshal(map[string]string{"base": server.URL, "password": testPassword})
	if err != nil {
		t.Fatal("browser config failed")
	}
	file := filepath.Join(t.TempDir(), "browser.json")
	if err := os.WriteFile(file, config, 0600); err != nil {
		t.Fatal("browser config write failed")
	}
	if _, err := exec.CommandContext(t.Context(), "node", script, file).CombinedOutput(); err != nil {
		t.Fatal("Pretendard browser failed; inspect font-metrics.json/status.json")
	}
}
