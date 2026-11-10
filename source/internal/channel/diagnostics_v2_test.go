package channel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestDiagnosticsUnknownSentinelsNeverReachAuthOrBugReport(t *testing.T) {
	auth, u := newDeviceAuthServer(t), newSwitchableUpstream(t)
	dir := t.TempDir()
	f := newDeviceModeFixture(t, auth, u, dir, singleIdentity())
	waitFor(t, "pending authorization", func() bool { return f.status("")["status"] == "pending" })
	s, err := clientstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	client, err := s.ClientID()
	if err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-4111-8111-111111111111"
	key := clientstate.SessionKey("https://broker/mcp?project=brk", "claude", id, client)
	now := time.Now().UTC().Add(24 * time.Hour)
	var events []map[string]any
	for i := 0; i < 25; i++ {
		events = append(events, map[string]any{"at": now.Add(time.Duration(i) * time.Second), "sessionId": id, "event": "private-secret-event", "reason": "private-secret-reason", "extension": map[string]any{"sentinel": "private-nested-sentinel"}})
	}
	obj := map[string]any{"summary": map[string]any{"lastFailureAt": now, "lastFailureClass": "private-secret-class", "extension": "private-summary-sentinel"}, "events": events, "extension": "private-top-sentinel"}
	raw, _ := json.Marshal(obj)
	legacy := filepath.Join(dir, "diagnostics", key+".json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, raw, 0600); err != nil {
		t.Fatal(err)
	}
	status := f.status("")
	summary, ok := status["diagnostics"].(map[string]any)
	if !ok || summary["lastFailureClass"] != "unknown" || summary["unknownCodes"].(float64) < 25 {
		t.Fatal("auth_status did not project/count unknown codes")
	}
	opts := durableOptions{state: s}
	locked := opts.localLockResult(t.Context(), unavailableUpstream{}, "auth_status", &localClientLockError{key: key})
	lockedJSON, _ := json.Marshal(locked)
	if strings.Contains(string(lockedJSON), "private-") {
		t.Fatal("local-lock auth_status leaked storage data")
	}
	visible, _ := json.Marshal(status)
	if strings.Contains(string(visible), "private-") {
		t.Fatal("auth_status leaked unknown storage data")
	}
	b := &ingressBinding{mu: &sync.Mutex{}, identity: broker.AttachRequest{Harness: "claude", SessionID: id}, durable: &durableRegistration{key: key}}
	diagnostics := opts.bugDiagnostics(t.Context(), b, unavailableUpstream{})
	if len(diagnostics.Events) != 20 {
		t.Fatal("bug report event cap changed")
	}
	for _, e := range diagnostics.Events {
		if e.Event != "unknown" || e.ReasonClass != "unknown" {
			t.Fatal("bug report did not retain generic occurrence")
		}
	}
	request := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{"title":"fixture","summary":"fixture"}`)}}
	ctx := context.WithValue(t.Context(), bugDiagnosticsKey{}, diagnostics)
	visible, err = bugReportArguments(ctx, request, unavailableUpstream{})
	if err != nil || strings.Contains(string(visible), "private-") || strings.Contains(string(visible), "unknownCodes") {
		t.Fatal("bug report leaked data or changed wire shape", err)
	}
}
