package controlpanel

import (
	"context"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionRuntimeInfoPanelRenderAndLivePermissions(t *testing.T) {
	now := time.Now().UTC()
	info := &protocol.RuntimeInfoSnapshot{RuntimeInfo: protocol.RuntimeInfo{Model: "gpt-6.1-sol", Effort: "medium"}, ReceivedAt: now, ExpiresAt: now.Add(time.Minute)}
	rows := []Session{{ID: "codex", RegistrationID: "codex", Name: "desk-codex-worker-2", SessionID: "11111111-1111-4111-8111-111111111111", Harness: "codex", Status: "alive", Ready: true, RuntimeInfo: info}}
	claude := *info
	claude.Model = "claude-opus-5-5"
	claude.Effort = "xhigh"
	rows = append(rows, Session{ID: "claude", RegistrationID: "claude", Name: "desk-claude-lead-1", Harness: "claude", Status: "alive", Ready: true, RuntimeInfo: &claude}, Session{ID: "old", RegistrationID: "old", Name: "old-client", Harness: "codex", Status: "alive", Ready: true})
	b := &runtimeFixtureBackend{liveFixtureBackend{rows: rows}}
	h := New(userStore(t), b, http.NotFoundHandler())
	c := loginPanelUser(t, h, "owner")
	page := c.call("GET", "/projects/sessions?project=demo", nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "gpt-6.1-sol / medium") || !strings.Contains(page.Body.String(), "claude-opus-5-5 / xhigh") || !strings.Contains(page.Body.String(), en("session.runtime.unknown")) {
		t.Fatal("panel runtime info rendering absent")
	}
	if dir := os.Getenv("ASB_MODEL_CAPTURE_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "sessions.html"), page.Body.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	viewer := loginPanelUser(t, h, "viewer")
	live := viewer.call("GET", "/projects/sessions/live?project=demo", nil)
	if live.Code != 200 || !strings.Contains(live.Body.String(), `"model":"gpt-6.1-sol"`) || !strings.Contains(live.Body.String(), `"effort":"medium"`) {
		t.Fatal("viewer model info absent")
	}
	outsider := loginPanelUser(t, h, "outsider")
	denied := outsider.call("GET", "/projects/sessions/live?project=demo", nil)
	if denied.Code != 403 || strings.Contains(denied.Body.String(), "gpt-6.1-sol") {
		t.Fatal("model metadata leaked to outsider")
	}
}

type runtimeFixtureBackend struct{ liveFixtureBackend }

func (b *runtimeFixtureBackend) Sessions(context.Context, string) ([]Session, error) {
	return b.rows, nil
}
