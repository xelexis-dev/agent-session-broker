package claude

import (
	"encoding/json"
	"errors"
	"testing"

	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestInspectParentKindsAndShortIDIsolation(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	for _, kind := range []string{"background", "interactive"} {
		t.Run(kind, func(t *testing.T) {
			raw, _ := json.Marshal([]map[string]any{{"id": "1234abcd", "sessionId": id, "pid": 123, "kind": kind, "cwd": "/work", "status": "idle"}})
			a := NewAdapter(&fakeExecutor{outputs: []string{string(raw)}})
			a.liveness = fakeLivenessProbe{alive: map[int]bool{123: true}}
			got, err := a.InspectParent(t.Context(), 123)
			if err != nil || got.ID != id || got.ConversationID != id || got.PID != 123 || got.CWD != "/work" {
				t.Fatalf("parent did not use UUID: %+v %v", got, err)
			}
		})
	}
}

func TestInspectParentGuards(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	for _, kind := range []string{"other-pid", "duplicate", "mixed-duplicate", "invalid-session", "unsupported", "no-cwd", "nul-cwd", "dead", "liveness-error", "invalid-pid"} {
		t.Run(kind, func(t *testing.T) {
			row := map[string]any{"id": "1234abcd", "sessionId": id, "pid": 123, "kind": "background", "cwd": "/work", "status": "idle"}
			rows := []map[string]any{row}
			switch kind {
			case "other-pid":
				row["pid"] = 124
			case "duplicate":
				rows = append(rows, row)
			case "mixed-duplicate":
				rows = append(rows, map[string]any{"pid": 123, "kind": "worker"})
			case "invalid-session":
				row["sessionId"] = "1234abcd"
			case "unsupported":
				row["kind"] = "worker"
			case "no-cwd":
				row["cwd"] = ""
			case "nul-cwd":
				row["cwd"] = "/work\x00"
			}
			raw, _ := json.Marshal(rows)
			a := NewAdapter(&fakeExecutor{outputs: []string{string(raw)}})
			a.liveness = fakeLivenessProbe{alive: map[int]bool{123: kind != "dead"}, errs: map[int]error{}}
			if kind == "liveness-error" {
				a.liveness = fakeLivenessProbe{errs: map[int]error{123: errors.New("unavailable")}}
			}
			pid := 123
			if kind == "invalid-pid" {
				pid = 0
			}
			if _, err := a.InspectParent(t.Context(), pid); err == nil {
				t.Fatal("unsafe parent accepted")
			}
		})
	}
}
func TestInspectBackgroundParentDialogIsLive(t *testing.T) {
	for _, status := range []string{"busy", "waiting", "idle", "working"} {
		t.Run(status, func(t *testing.T) {
			raw, _ := json.Marshal([]map[string]any{{"id": "1234abcd", "sessionId": "11111111-1111-4111-8111-111111111111", "pid": 123, "kind": "background", "cwd": "/work", "status": status}})
			a := NewAdapter(&fakeExecutor{outputs: []string{string(raw)}})
			a.liveness = fakeLivenessProbe{alive: map[int]bool{123: true}}
			if _, err := a.InspectParent(t.Context(), 123); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInspectBackgroundParentUsesStatusDespiteState(t *testing.T) {
	for _, state := range []string{"done", "blocked", "working"} {
		t.Run(state, func(t *testing.T) {
			raw, _ := json.Marshal([]map[string]any{{"id": "1234abcd", "sessionId": "11111111-1111-4111-8111-111111111111", "pid": 123, "kind": "background", "cwd": "/work", "status": "idle", "state": state}})
			a := NewAdapter(&fakeExecutor{outputs: []string{string(raw)}})
			a.liveness = fakeLivenessProbe{alive: map[int]bool{123: true}}
			got, err := a.InspectParent(t.Context(), 123)
			if err != nil || got.Status != broker.StatusIdle {
				t.Fatalf("live idle parent state=%s: status=%s err=%v", state, got.Status, err)
			}
		})
	}
}
func TestInspectBackgroundStoppedParentStillNeedsLiveProcess(t *testing.T) {
	raw, _ := json.Marshal([]map[string]any{{"id": "1234abcd", "sessionId": "11111111-1111-4111-8111-111111111111", "pid": 123, "kind": "background", "cwd": "/work", "status": "idle", "state": "stopped"}})
	a := NewAdapter(&fakeExecutor{outputs: []string{string(raw)}})
	a.liveness = fakeLivenessProbe{alive: map[int]bool{123: false}}
	if _, err := a.InspectParent(t.Context(), 123); err == nil {
		t.Fatal("dead parent accepted")
	}
}
func TestInspectInteractiveParentPreservesStatePrecedence(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	for _, state := range []string{"done", "blocked", "working"} {
		t.Run(state, func(t *testing.T) {
			raw, _ := json.Marshal([]map[string]any{{"id": id, "sessionId": id, "pid": 123, "kind": "interactive", "name": "fixture", "cwd": "/work", "status": "idle", "state": state}})
			a := NewAdapter(&fakeExecutor{outputs: []string{string(raw)}})
			a.liveness = fakeLivenessProbe{alive: map[int]bool{123: true}}
			got, err := a.InspectParent(t.Context(), 123)
			wantStatus := map[string]broker.SessionStatus{"done": "", "blocked": broker.StatusIdle, "working": broker.StatusWorking}[state]
			if errors.Is(err, ErrSessionNotFound) != (state == "done") || got.Status != wantStatus {
				t.Fatalf("interactive state=%s got=%s/%v want=%s", state, got.Status, err, wantStatus)
			}
		})
	}
}

func TestInteractiveDialogKeepsLiveIdentity(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	for _, status := range []string{"waiting", "busy"} {
		raw, _ := json.Marshal([]map[string]any{{"sessionId": id, "kind": "interactive", "name": "test", "pid": 123, "cwd": t.TempDir(), "status": status, "waitingFor": "dialog open"}})
		for _, alive := range []bool{true, false} {
			a := NewAdapter(&fakeExecutor{outputs: []string{string(raw)}})
			a.liveness = fakeLivenessProbe{alive: map[int]bool{123: alive}}
			got, err := a.InspectParent(t.Context(), 123)
			if alive && (err != nil || got.Status != broker.StatusWorking || got.PID != 123) {
				t.Fatalf("live dialog: %+v %v", got, err)
			}
			if !alive && err == nil {
				t.Fatal("dead dialog accepted")
			}
		}
	}
}
