package controlpanel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestUICopyFullChrome(t *testing.T) {
	script := os.Getenv("ASB_UI_COPY_BROWSER")
	if script == "" {
		t.Skip("opt-in full Chrome")
	}
	s := userStore(t)
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/capture/issued" {
			v := uiCopyPreview("settings")
			v.IssuedToken = "SYNTHETIC-PREVIEW-NOT-A-CREDENTIAL"
			render(w, 200, v)
			return
		}
		if r.URL.Path == "/capture/authcode" {
			v := uiCopyPreview("authcode")
			v.Code = "TEST-CODE"
			v.Device = &DeviceView{Project: "demo", OwnerID: "owner", Harness: "codex", ClientID: "synthetic-client", SessionID: "11111111-1111-4111-8111-111111111111", InstanceID: "synthetic-pc", CWDBasename: "demo", Status: "pending", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(10 * time.Minute)}
			render(w, 200, v)
			return
		}
		h.ServeHTTP(w, r)
	}))
	defer server.Close()
	setupStore, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer setupStore.Close()
	setup := httptest.NewServer(New(setupStore, tokenViewBackend{}, http.NotFoundHandler()))
	defer setup.Close()
	config, err := json.Marshal(map[string]string{"base": server.URL, "setup": setup.URL, "password": testPassword})
	if err != nil {
		t.Fatal("config failed")
	}
	file := filepath.Join(t.TempDir(), "browser.json")
	if os.WriteFile(file, config, 0600) != nil {
		t.Fatal("config write failed")
	}
	cmd := exec.CommandContext(t.Context(), "node", script, file)
	if _, err := cmd.CombinedOutput(); err != nil {
		t.Fatal("UI copy browser assertions failed; output withheld")
	}
}
