package channel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

func TestRound4UnknownV2DiagnosticsSentToServerOnlyAsUnknown(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	h := r.harness(t, dir, 9404)
	h.attach(t)
	state, err := clientstate.Open(dir)
	if err != nil {
		t.Fatal("fixture state")
	}
	client, err := state.ClientID()
	if err != nil {
		t.Fatal("fixture client")
	}
	id := "11111111-1111-4111-8111-000000009404"
	key := clientstate.SessionKey(brokerScope(r.endpoint, "detach"), "codex", id, client)
	sentinel := "secretlike-round4-sentinel"
	raw, _ := json.Marshal(map[string]any{"format": 2, "extension": map[string]any{"private": sentinel}, "summary": map[string]any{"lastFailureClass": sentinel, "extension": sentinel}, "events": []any{map[string]any{"at": time.Now().UTC().Add(time.Hour), "sessionId": id, "event": sentinel, "reason": sentinel, "extension": sentinel}}})
	path := filepath.Join(dir, "diagnostics", "v2", key+".json")
	if os.MkdirAll(filepath.Dir(path), 0700) != nil || os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("fixture diagnostics")
	}
	status := h.status(t)
	visible, _ := json.Marshal(status)
	if strings.Contains(string(visible), sentinel) {
		t.Fatal("auth_status leaked sentinel")
	}
	result, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "report_bug", Arguments: map[string]any{"title": "round4 fixture", "summary": "safe fixture"}})
	if err != nil || result.IsError {
		t.Fatal("fixture report rejected")
	}
	var response struct {
		ID string `json:"id"`
	}
	visible, _ = json.Marshal(result.StructuredContent)
	if json.Unmarshal(visible, &response) != nil || response.ID == "" {
		t.Fatal("report response missing")
	}
	db, err := sqlitestore.Open(filepath.Join(r.root, "bugreports"), "bugreports.db")
	if err != nil {
		t.Fatal("fixture database")
	}
	defer db.Close()
	stored, err := db.Get(response.ID)
	if err != nil || strings.Contains(string(stored), sentinel) {
		t.Fatal("server stored raw unknown data")
	}
	var report struct {
		Client struct {
			Events []struct {
				Event  string `json:"event"`
				Reason string `json:"reasonClass"`
			} `json:"events"`
		} `json:"client"`
	}
	if json.Unmarshal(stored, &report) != nil {
		t.Fatal("report decode")
	}
	found := false
	for _, e := range report.Client.Events {
		if e.Event == "unknown" && e.Reason == "unknown" {
			found = true
		}
	}
	if !found {
		t.Fatal("server did not receive generic unknown occurrence")
	}
}
