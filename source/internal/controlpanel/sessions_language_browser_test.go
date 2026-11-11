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

	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// Full Chrome: the session pages and live updates in English and Korean (see sessions_language_browser.cjs).
func TestSessionsLanguageBrowserFullChrome(t *testing.T) {
	if os.Getenv("ASB_SESSIONS_LANGUAGE_BROWSER") == "" {
		t.Skip("opt-in sessions language full Chrome")
	}
	s := userStore(t)
	if err := s.UpdateLanguage("developer", "ko"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetScopeIssuanceReady("admin", true); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.mu.Lock()
	s.audits["sessions-language"] = AuditEvent{Project: "demo", ProjectGenerationID: s.data.Projects["demo"].ProjectGenerationID, OwnerID: "developer", Kind: "registration", Reason: "confirmed", At: now.UTC()}
	s.mu.Unlock()
	backend := &readyBackend{liveFixtureBackend{rows: []Session{
		{ID: "ready", RegistrationID: "ready", Name: "ready-session", Harness: "claude", Status: "alive", Ready: true, PID: 4242,
			Activity:    &protocol.ActivitySnapshot{Activity: protocol.Activity{State: protocol.ActivityWorking, Source: "claude-session-file", Sequence: 1, ObservedAt: now}, ReceivedAt: now, ExpiresAt: now.Add(time.Hour)},
			RuntimeInfo: &protocol.RuntimeInfoSnapshot{RuntimeInfo: protocol.RuntimeInfo{Model: "claude-opus-5-5", Effort: "high"}, ReceivedAt: now, ExpiresAt: now.Add(time.Hour)}},
		{ID: "lost", RegistrationID: "lost", Name: "lost-session", Harness: "codex", Status: "missing", ErrorCode: "ingress-failed", ProbePending: true},
	}}}
	server := httptest.NewServer(New(s, backend, http.NotFoundHandler()))
	defer server.Close()
	data, _ := json.Marshal(map[string]string{"base": server.URL, "password": testPassword})
	config := filepath.Join(t.TempDir(), "sessions-language-browser.json")
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "node", "sessions_language_browser.cjs", config)
	if err := cmd.Run(); err != nil {
		t.Fatal("sessions language browser/axe check failed; inspect sanitized evidence")
	}
}
