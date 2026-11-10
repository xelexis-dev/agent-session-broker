package controlpanel

import (
	"context"
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

type sessionPIDBrowserBackend struct{ layoutBackend }

func (sessionPIDBrowserBackend) Sessions(context.Context, string) ([]Session, error) {
	runtime := &protocol.RuntimeInfoSnapshot{RuntimeInfo: protocol.RuntimeInfo{Model: "synthetic-model", Effort: "high", Sequence: 1, Generation: 1, ObservedAt: time.Now()}, ReceivedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	return []Session{
		{ID: "synthetic-registration-a", SessionID: "11111111-1111-4111-8111-111111111111", ClientID: "synthetic-pc", Name: "worker-normal", PID: 4242, Harness: "codex", Status: "alive", Ready: true, RuntimeInfo: runtime},
		{ID: "synthetic-registration-b", SessionID: "22222222-2222-4222-8222-222222222222", ClientID: "synthetic-pc", Name: "worker-with-a-long-session-name-for-mobile-wrapping-and-runtime-details", PID: 5151, Harness: "claude", Status: "alive", Ready: true, RuntimeInfo: runtime},
		{ID: "synthetic-registration-c", SessionID: "33333333-3333-4333-8333-333333333333", ClientID: "synthetic-pc", Name: "worker-no-pid", Harness: "codex", Status: "missing"},
	}, nil
}
func TestSessionPIDFullChrome(t *testing.T) {
	script := os.Getenv("ASB_SESSION_PID_BROWSER")
	if script == "" {
		t.Skip("opt-in full Chrome")
	}
	server := httptest.NewServer(New(userStore(t), sessionPIDBrowserBackend{}, http.NotFoundHandler()))
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
		t.Fatal("session PID browser failed; inspect boxes.json")
	}
}
