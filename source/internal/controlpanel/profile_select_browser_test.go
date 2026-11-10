package controlpanel

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Deterministic view data keeps the selected draft comparison independent of auth;
// api_panel_test and profile_http_test exercise the authenticated handlers.
func TestProfileSelectFullChrome(t *testing.T) {
	if os.Getenv("ASB_PROFILE_SELECT_BROWSER") == "" {
		t.Skip("opt-in selected profile and select comparison")
	}
	s := userStore(t)
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	ln, err := net.Listen("tcp", "127.0.0.1:18897")
	if err != nil {
		t.Fatal("spare port unavailable")
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 8 && r.URL.Path[:8] == "/assets/" {
			h.ServeHTTP(w, r)
			return
		}
		if r.Method != "GET" {
			http.Error(w, "synthetic read-only fixture", 405)
			return
		}
		role, state := r.URL.Query().Get("role"), r.URL.Query().Get("state")
		view := map[string]string{"/profile": "profile", "/profile/api-tokens": "api-tokens", "/profile/api-tokens/new": "api-token-new", "/admin/bug-reports": "bug-reports"}[r.URL.Path]
		if view == "" {
			http.NotFound(w, r)
			return
		}
		user := User{ID: "admin", Username: "admin", Role: "admin", Enabled: true}
		if role == "user" {
			user = User{ID: "agent-bot", Username: "agent-bot", Role: "user", Enabled: true}
		}
		v := page{View: view, User: user, CSRF: "synthetic-csrf", Form: url.Values{}, SettingsVersions: map[string]string{}, AdminAPIEnabled: true, AdminView: view == "bug-reports"}
		v.ProfileProjects = []profileProject{{"data-pipeline-nightly-batch-jobs-long-name", "Owner"}, {"demo-project", "Owner"}, {"docs-site", "Owner"}, {"infra-tools", "Owner"}, {"mobile-app", "Owner"}}
		if role == "user" {
			v.ProfileProjects = []profileProject{{"demo-project", "Developer"}, {"mobile-app", "Viewer"}}
		}
		if state == "empty" {
			v.ProfileProjects = nil
		}
		if state == "many" {
			v.ProfileProjects = []profileProject{{"analytics-dashboard", "Owner"}, {"billing-service", "Developer"}, {"data-pipeline-nightly-batch-jobs-long-name", "Owner"}, {"demo-project", "Developer"}, {"docs-site", "Viewer"}, {"edge-gateway", "Developer"}, {"infra-tools", "Owner"}, {"internal-wiki", "Viewer"}, {"ml-training-cluster", "Developer"}, {"mobile-app", "Viewer"}, {"payments-api", "Developer"}, {"release-bot", "Owner"}}
		}
		if state == "err-http" {
			v.APIError = "http"
			v.APIIssueDisabled = true
			v.APIHTTPRefused = true
		}
		if state == "err-cap" {
			v.APIError = "cap"
			v.APIIssueDisabled = true
		}
		if err := pages.ExecuteTemplate(w, "page", v); err != nil {
			t.Error("visual render failed")
		}
	})}
	go func() { _ = server.Serve(ln) }()
	defer server.Close()
	config := filepath.Join(t.TempDir(), "visual.json")
	data, _ := json.Marshal(map[string]string{"Base": "http://127.0.0.1:18897"})
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := exec.CommandContext(t.Context(), "node", "profile_select_browser.cjs", config).Run(); err != nil {
		t.Fatal("profile/select browser check failed; inspect sanitized evidence")
	}
}
