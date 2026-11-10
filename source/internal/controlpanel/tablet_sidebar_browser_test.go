package controlpanel

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestTabletSidebarFullChrome(t *testing.T) {
	if os.Getenv("ASB_PANEL_TABLET_BROWSER") == "" {
		t.Skip("opt-in synthetic tablet sidebar Chrome check")
	}
	s := userStore(t)
	if err := s.CreateProjectFor("admin", "tablet-demo"); err != nil {
		t.Fatal("project fixture")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:18897")
	if err != nil {
		t.Fatal("spare browser port unavailable")
	}
	server := &http.Server{Handler: New(s, &panelBackend{}, http.NotFoundHandler())}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	dir := t.TempDir()
	password := filepath.Join(dir, "password")
	if err = os.WriteFile(password, []byte(testPassword), 0600); err != nil {
		t.Fatal("password fixture")
	}
	raw, _ := json.Marshal(map[string]string{"Base": "http://127.0.0.1:18897", "PasswordFile": password})
	config := filepath.Join(dir, "browser-private.json")
	if err = os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal("browser config")
	}
	if err = exec.CommandContext(t.Context(), "node", "tablet_sidebar_browser.cjs", config).Run(); err != nil {
		t.Fatal("tablet sidebar browser assertions failed")
	}
}
