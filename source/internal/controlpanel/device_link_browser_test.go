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

// Opt-in full Chrome check against this real panel and isolated synthetic data.
func TestDeviceApprovalFullChrome(t *testing.T) {
	script := os.Getenv("ASB_DEVICE_LINK_BROWSER")
	if script == "" {
		t.Skip("opt-in full Chrome")
	}
	f := newDeviceHTTPFixture(t)
	owner := loginPanelUser(t, f.h, "owner")
	token := issuedTokenFrom(t, owner.call("GET", issueProjectResult(t, owner, "browser fixture"), nil))
	form := deviceForm("11111111-1111-4111-8111-111111111111", "browser-link")
	form.Set("cwd_basename", "approval-project")
	_, issued := f.rest("POST", "/oauth/device_authorization?project=demo", token, form)
	server := httptest.NewServer(f.h)
	defer server.Close()
	settings := map[string]string{"base": server.URL, "link": server.URL + issued["verification_uri_complete"].(string) + "&name=" + url.QueryEscape("browser-worker"), "username": "owner", "password": testPassword, "code": issued["user_code"].(string)}
	file := filepath.Join(t.TempDir(), "browser.json")
	data, _ := json.Marshal(settings)
	if e := os.WriteFile(file, data, 0600); e != nil {
		t.Fatal(e)
	}
	cmd := exec.CommandContext(t.Context(), "node", script, file)
	if _, e := cmd.CombinedOutput(); e != nil {
		t.Fatal("full Chrome fixture failed (output withheld to protect approval details)")
	}
	view, e := f.store.DeviceAuthorizationFor("owner", settings["code"])
	if e != nil || view.Status != "approved" || view.Nickname != "browser-worker" {
		t.Fatal("browser did not approve exactly once")
	}
}
