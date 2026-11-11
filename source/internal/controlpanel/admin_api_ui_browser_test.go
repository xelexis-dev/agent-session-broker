package controlpanel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The visual fixture uses production templates with deterministic view data;
// api_panel_test exercises the authenticated handlers and receipt consumption.
func TestAdminAPIUIFullChrome(t *testing.T) {
	if os.Getenv("ASB_ADMIN_API_UI_BROWSER") == "" {
		t.Skip("opt-in selected management API design comparison")
	}
	s := userStore(t)
	useKorean(t, s)
	assetsHandler := New(s, &panelBackend{}, http.NotFoundHandler())
	parse := func(raw string) time.Time {
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			t.Fatal("fixture date")
		}
		return at
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			assetsHandler.ServeHTTP(w, r)
			return
		}
		q := r.URL.Query()
		state, role, view := q.Get("state"), q.Get("role"), q.Get("view")
		if r.Method != "GET" {
			http.Error(w, "synthetic fixture is read-only", 405)
			return
		}
		v := page{View: view, Version: "0.18.1", User: User{ID: "admin", Username: "admin", Role: role, Enabled: true}, CSRF: "synthetic-csrf", Form: url.Values{}, SettingsVersions: map[string]string{}, AdminAPIEnabled: state != "off", AdminView: view == "admin-settings" || view == "user-detail", SelectedUser: User{ID: "agent-bot", Username: "agent-bot", Role: "user", Enabled: true}, ProfileProjects: []profileProject{{Name: "demo-project", Role: "Owner"}}, Projects: []Project{{Name: "demo-project", Enabled: true}}, AttachmentDisk: attachmentDiskView{Known: true, Free: "146.2", Total: "460.4"}, AttachmentMaxMiB: 100, AttachmentExpiryMinutes: 30}
		if role == "user" {
			v.User.ID = "worker"
			v.User.Username = "worker"
		}
		if view == "admin-settings" {
			v.AdminAPIEnabled = state == "on"
			at := "2026-11-05T14:20:00+09:00"
			if v.AdminAPIEnabled {
				at = "2026-11-05T14:32:00+09:00"
			}
			v.AdminAPILastChange = &ScopeIssuanceChange{Actor: "admin", At: parse(at), Old: policyBool(!v.AdminAPIEnabled), New: policyBool(v.AdminAPIEnabled)}
		}
		if strings.HasPrefix(state, "err-") {
			v.APIError = strings.TrimPrefix(state, "err-")
			v.APIIssueDisabled = v.APIError != "password"
			v.APIHTTPRefused = v.APIError == "http"
		}
		if state == "issued" {
			v.IssuedAPIToken = "asb_" + "u_EXAMPLE…"
		}
		if state == "tokens" || state == "issued" || state == "off" && view == "api-tokens" {
			v.UserAPITokens = []UserTokenView{
				{ID: "token_demo_1", Label: "배포 자동화", Scopes: []string{"read", "write"}, Source: "panel", CreatedAt: parse("2026-10-06T09:12:00+09:00"), ExpiresAt: parse("2026-11-05T09:12:00+09:00"), LastUsedAt: parse("2026-11-04T13:42:00+09:00")},
				{ID: "token_demo_2", Label: "production-cluster-프로젝트-배포-및-세션-관리를-위한-에이전트-자동화-토큰-2026-하반기", Scopes: []string{"read", "write", "approve"}, Source: "login", CreatedAt: parse("2026-10-29T18:05:00+09:00"), ExpiresAt: parse("2026-11-28T18:05:00+09:00")},
				{ID: "token_demo_3", Label: "로컬 점검", Scopes: []string{"read"}, Source: "panel", CreatedAt: parse("2026-11-04T11:30:00+09:00"), ExpiresAt: parse("2026-11-11T11:30:00+09:00"), LastUsedAt: parse("2026-11-05T08:17:00+09:00")},
			}
		}
		if err := koPages.ExecuteTemplate(w, "page", v); err != nil {
			t.Error("visual fixture render")
		}
	}))
	defer server.Close()
	raw, _ := json.Marshal(map[string]string{"Base": server.URL})
	config := filepath.Join(t.TempDir(), "visual.json")
	if err := os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal("fixture config")
	}
	if err := exec.CommandContext(t.Context(), "node", "admin_api_ui_browser.cjs", config).Run(); err != nil {
		t.Fatal("management API browser assertions failed; inspect sanitized evidence")
	}
}
