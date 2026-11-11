package controlpanel

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func TestTokenScopeEditorFullChrome(t *testing.T) {
	if os.Getenv("ASB_SCOPE_PANEL_BROWSER") == "" {
		t.Skip("opt-in full Chrome")
	}
	s := userStore(t)
	useKorean(t, s)
	if err := s.SetScopeIssuanceReady("admin", true); err != nil {
		t.Fatal(err)
	}
	baseline := []string{access.MemoryRead, access.MessageSend, access.SessionRead, access.SessionRegister, access.SessionRename}
	var sessions []Session
	parents := map[string]string{}
	for _, actor := range []string{"owner", "developer"} {
		raw, err := s.IssueProjectTokenWithOptionsFor(actor, "demo", map[string]string{"owner": "dev-agent", "developer": "review-agent"}[actor], ProjectTokenOptions{Scopes: baseline})
		if err != nil {
			t.Fatal(err)
		}
		parent := scopeEditParent(t, s, raw)
		parents[actor] = parent.ID
		for i := 0; i < 4; i++ {
			req := DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", InstanceID: fmt.Sprintf("%s-client-%d", actor, i), SessionID: fmt.Sprintf("%s-session-%d", actor, i), RequestID: fmt.Sprintf("%s-request-%d", actor, i)}
			device, err := s.BeginDeviceAuthorization("demo", raw, req)
			if err != nil {
				t.Fatal(err)
			}
			name := fmt.Sprintf("%s-worker-%d", actor, i)
			if _, err := s.ApproveDeviceAuthorization("owner", device.UserCode, name, false); err != nil {
				t.Fatal(err)
			}
			if i < 3 {
				sessions = append(sessions, Session{ID: name, Name: name, Status: "alive", Harness: req.Harness, SessionID: req.SessionID, ClientID: req.InstanceID, OwnerID: actor})
			}
		}
	}

	autoScopes := append(slices.Clone(baseline), access.SessionRegisterAuto)
	autoRaw, err := s.IssueProjectTokenWithOptionsFor("owner", "demo", "automation", ProjectTokenOptions{Scopes: autoScopes, MaxAutoBindings: 10, AutoLifetimeDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	auto := scopeEditParent(t, s, autoRaw)
	if err := s.CreateProjectFor("owner", "scope-edges"); err != nil {
		t.Fatal(err)
	}
	edges := map[string]string{}
	for key, label := range map[string]string{"all": "all-scopes", "long": strings.Repeat("LongTokenLabel", 9)} {
		raw, err := s.IssueProjectTokenWithOptionsFor("owner", "scope-edges", label, ProjectTokenOptions{Scopes: []string{access.SessionRegister}})
		if err != nil {
			t.Fatal(err)
		}
		edges[key] = scopeEditParent(t, s, raw).ID
	}

	historyRaw, err := s.IssueProjectTokenWithOptionsFor("owner", "scope-edges", "history-only", ProjectTokenOptions{Scopes: []string{access.MessageReceive, access.SessionRegister}})
	if err != nil {
		t.Fatal(err)
	}
	history := scopeEditParent(t, s, historyRaw)
	device, err := s.BeginDeviceAuthorization("scope-edges", historyRaw, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", InstanceID: "history-pc", SessionID: "history-session", RequestID: "history-request"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApproveDeviceAuthorization("owner", device.UserCode, "history-worker", false); err != nil {
		t.Fatal(err)
	}
	p := s.data.Projects["scope-edges"]
	for i := range p.Tokens {
		if p.Tokens[i].ParentID == history.ID {
			p.Tokens[i].Revoked = true
			p.Tokens[i].Scopes = []string{access.MessageReceive}
		}
	}
	if err := s.commitProject(p); err != nil {
		t.Fatal(err)
	}
	h := New(s, tokenViewBackend{sessions: sessions}, http.NotFoundHandler(), "0.17.2-dev")
	owner := loginPanelUser(t, h, "owner")
	developer := loginPanelUser(t, h, "developer")
	admin := loginPanelUser(t, h, "admin")
	viewer := loginPanelUser(t, h, "viewer")
	server := httptest.NewServer(h)
	defer server.Close()
	digest := sha256.New()
	for _, name := range []string{"panel.js", "panel.css", "templates.html", "token-scope-editor.html", "token_scope_panel.go", "token_scope_edit.go", "token_scope_edit_http.go"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		digest.Write(data)
	}
	config := map[string]string{"Base": server.URL, "CookieName": owner.cookie.Name, "OwnerCookie": owner.cookie.Value, "DeveloperCookie": developer.cookie.Value, "AdminCookie": admin.cookie.Value, "ViewerCookie": viewer.cookie.Value, "OwnerID": parents["owner"], "DeveloperID": parents["developer"], "AutoID": auto.ID, "AllID": edges["all"], "LongID": edges["long"], "HistoryID": history.ID, "SourceDigest": fmt.Sprintf("%x", digest.Sum(nil))}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(t.TempDir(), "scope-panel-private.json")
	if err := os.WriteFile(private, raw, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "node", "scope_editor_browser.cjs", private)
	// The harness prints only test stages and non-secret evidence paths.
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		t.Fatal("scope editor full Chrome assertions failed")
	}
}
