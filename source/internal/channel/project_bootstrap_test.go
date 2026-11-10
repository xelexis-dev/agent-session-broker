package channel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
)

func TestProjectTerminalBackgroundBootstrapKeepsDiscoveryAndGuidance(t *testing.T) {
	parent := duplicateParent(t)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(410)
		w.Write([]byte(`{"error":"project_deleted"}`))
	}))
	defer server.Close()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	endpoint := server.URL + "/mcp?project=p"
	dir := t.TempDir()
	credential := "synthetic-bootstrap-credential"
	state, err := clientstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tracker, err := newProjectTracker(state, brokerScope(endpoint, "p"))
	if err != nil {
		t.Fatal(err)
	}
	tracker.learn(credentialSource(credential), "", strings.Repeat("A", 26), true)
	client := startupClient(t, []string{"--broker-url", endpoint, "--token", credential, "--state-dir", dir}, parent)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("background bootstrap never started")
	}
	// The upstream is still blocked: both discovery requests already completed.
	if _, err := client.ListTools(t.Context(), nil); err != nil {
		t.Fatal("local catalog blocked on bootstrap")
	}
	unblock()
	waitFor(t, "authenticated project terminal saved", func() bool {
		saved, err := newProjectTracker(state, brokerScope(endpoint, "p"))
		return err == nil && terminalProjectError(saved.terminal(credentialSource(credential)))
	})
	var status authStatus
	if err := call(t.Context(), client, "auth_status", struct{}{}, &status); err != nil {
		t.Fatal("auth_status unavailable after bootstrap terminal")
	}
	if status.Registered || !strings.Contains(status.Message, "project_deleted") || !strings.Contains(status.Message, "preserved") {
		t.Fatal("background project terminal missing from auth_status")
	}
	result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_sessions", Arguments: struct{}{}})
	found := err != nil && strings.Contains(err.Error(), "project_deleted")
	if err == nil && result != nil && result.IsError {
		for _, c := range result.Content {
			if text, ok := c.(*mcp.TextContent); ok && strings.Contains(text.Text, "project_deleted") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("tool result lost project guidance")
	}
	if requests.Load() != 1 {
		t.Fatal("terminal bootstrap made additional broker requests")
	}
	if _, err := client.ListTools(t.Context(), nil); err != nil {
		t.Fatal("catalog stopped after project terminal")
	}
}
