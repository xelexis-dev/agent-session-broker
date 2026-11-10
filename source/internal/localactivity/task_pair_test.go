package localactivity

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Captured from our normal scratch turn, vscode 0.162.1, 2026-10-10.
// Only event type, timestamps and a substituted same-turn identifier remain.
func normalTurn(t *testing.T) (string, string, time.Time) {
	t.Helper()
	raw, err := os.ReadFile("testdata/codex-normal-selected.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	if len(lines) != 2 {
		t.Fatal("expected selected start/end")
	}
	var end struct {
		Timestamp time.Time `json:"timestamp"`
	}
	if err := json.Unmarshal(lines[1], &end); err != nil {
		t.Fatal(err)
	}
	return string(lines[0]) + "\n", string(lines[1]) + "\n", end.Timestamp
}

func taskPairFixture(t *testing.T) *fixture {
	f := newFixture(t, "codex")
	f.config.Format = "codex-task-pair-v1"
	f.observer = New(f.config)
	return f
}

func TestCapturedCodexTaskPairStartEnd(t *testing.T) {
	start, end, at := normalTurn(t)
	f := taskPairFixture(t)
	f.now = at
	f.write(f.meta(sessionA) + start)
	if got := f.sample(); got.State != Working {
		t.Fatalf("captured start: state=%s reason=%s; want working", got.State, got.Reason)
	}
	f.append(end)
	if got := f.sample(); got.State != Idle {
		t.Fatalf("captured matching end: state=%s reason=%s; want idle", got.State, got.Reason)
	}
}

func TestTaskPairSchemaUnknownFallback(t *testing.T) {
	for _, tc := range []struct{ name, middle string }{
		{"mismatched end", `{"type":"event_msg","timestamp":"2026-10-10T07:22:05.974Z","payload":{"type":"task_complete","turn_id":"other-turn"}}`},
		{"unknown event", `{"type":"event_msg","payload":{"type":"future_event","turn_id":"normal-turn"}}`},
		{"missing turn id", `{"type":"event_msg","timestamp":"2026-10-10T07:22:05.974Z","payload":{"type":"task_complete"}}`},
		{"interrupt", `{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"normal-turn"}}`},
		{"approval", `{"type":"event_msg","payload":{"type":"exec_approval_request","turn_id":"normal-turn"}}`},
		{"tool", `{"type":"response_item","payload":{"type":"function_call"}}`},
		{"message", `{"type":"response_item","payload":{"type":"message"}}`},
		{"missing schema", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, end, at := normalTurn(t)
			f := taskPairFixture(t)
			f.now = at
			f.write(f.meta(sessionA) + start)
			expectState(t, f.sample(), Working)
			f.append(tc.middle + "\n")
			expectState(t, f.sample(), Unknown)
			f.append(end)
			want := Idle
			want = Unknown                   // Unknown schema, missing IDs and abort discard the open pair.
			expectState(t, f.sample(), want) // Only the explicit same-turn completion restores idle.
			f.now = f.now.Add(time.Second)
			f.append(f.event("task_started", "fresh") + f.event("task_complete", "fresh"))
			expectState(t, f.sample(), Idle) // A later fresh pair restores evidence.
		})
	}
	t.Run("end without start", func(t *testing.T) {
		_, end, at := normalTurn(t)
		f := taskPairFixture(t)
		f.now = at
		f.write(f.meta(sessionA) + end)
		expectState(t, f.sample(), Unknown)
	})
	t.Run("stale", func(t *testing.T) {
		start, _, at := normalTurn(t)
		f := taskPairFixture(t)
		f.now = at
		f.write(f.meta(sessionA) + start)
		expectState(t, f.sample(), Working)
		f.now = f.now.Add(5 * time.Minute)
		expectState(t, f.sample(), Unknown)
	})
	t.Run("identity", func(t *testing.T) {
		start, _, at := normalTurn(t)
		f := taskPairFixture(t)
		f.now = at
		f.write(f.meta(sessionB) + start)
		expectState(t, f.sample(), Unknown)
	})
	t.Run("budget", func(t *testing.T) {
		start, _, at := normalTurn(t)
		f := taskPairFixture(t)
		f.now = at
		f.write(f.meta(sessionA) + start + string(bytes.Repeat([]byte("x"), 1<<20)))
		got := f.sample()
		if got.State != Unknown || got.Reason != "budget-exceeded" {
			t.Fatalf("budget sample: %+v", got)
		}
	})
	t.Run("future producer metadata", func(t *testing.T) {
		start, end, at := normalTurn(t)
		f := taskPairFixture(t)
		f.now = at
		var meta map[string]any
		_ = json.Unmarshal([]byte(f.meta(sessionA)), &meta)
		meta["payload"].(map[string]any)["cli_version"] = "999.0.0"
		raw, _ := json.Marshal(meta)
		f.write(string(raw) + "\n" + start + end)
		expectState(t, f.sample(), Idle)
	})
}

func TestTaskPairCompletionCannotGoBackwardsAcrossUnknownPhase(t *testing.T) {
	f := taskPairFixture(t)
	f.write(f.meta(sessionA) + f.event("task_started", "a"))
	expectState(t, f.sample(), Working)
	f.append(`{"type":"response_item","payload":{"type":"message"}}` + "\n")
	expectState(t, f.sample(), Unknown)
	f.now = f.now.Add(-time.Second)
	f.append(f.event("task_complete", "a"))
	expectState(t, f.sample(), Unknown)
}

func TestOpenTaskToolMessageProgressStaysWorking(t *testing.T) {
	f := taskPairFixture(t)
	f.write(f.meta(sessionA) + f.event("task_started", "a"))
	expectState(t, f.sample(), Working)
	f.now = f.now.Add(4 * time.Minute)
	f.append(f.event("exec_command_begin", "a"))
	if got := f.sample(); got.State != Working {
		t.Fatalf("tool progress: %s/%s; want working", got.State, got.Reason)
	}
	f.now = f.now.Add(4 * time.Minute)
	f.append(f.event("agent_message", "a"))
	expectState(t, f.sample(), Working)
	f.append(f.event("task_complete", "a"))
	expectState(t, f.sample(), Idle)
}

func TestTaskPairApprovalAbortAndSilentTool(t *testing.T) {
	t.Run("approval then progress", func(t *testing.T) {
		f := taskPairFixture(t)
		f.write(f.meta(sessionA) + f.event("task_started", "a"))
		expectState(t, f.sample(), Working)
		f.now = f.now.Add(time.Minute)
		f.append(f.event("exec_approval_request", "a"))
		expectState(t, f.sample(), Unknown)
		f.now = f.now.Add(time.Minute)
		f.append(f.event("agent_reasoning", "a"))
		expectState(t, f.sample(), Working)
	})
	t.Run("abort closes turn", func(t *testing.T) {
		f := taskPairFixture(t)
		f.write(f.meta(sessionA) + f.event("task_started", "a"))
		expectState(t, f.sample(), Working)
		f.append(f.event("turn_aborted", "a"))
		expectState(t, f.sample(), Unknown)
		f.append(f.event("agent_message", "a") + f.event("task_complete", "a"))
		expectState(t, f.sample(), Unknown)
	})
	t.Run("silent tool", func(t *testing.T) {
		f := taskPairFixture(t)
		f.write(f.meta(sessionA) + f.event("task_started", "a"))
		f.now = f.now.Add(time.Minute)
		f.append(f.event("exec_command_begin", "a"))
		expectState(t, f.sample(), Working)
		f.now = f.now.Add(5 * time.Minute)
		got := f.sample()
		if got.State != Unknown || got.Reason != "stale-evidence" {
			t.Fatalf("silent tool: %+v", got)
		}
		f.append(f.event("exec_command_end", "a"))
		expectState(t, f.sample(), Working)
		f.append(f.event("task_complete", "a"))
		expectState(t, f.sample(), Idle)
	})
	for _, kind := range []string{"agent_message", "agent_reasoning", "exec_command_begin", "exec_command_end", "mcp_tool_call_begin", "mcp_tool_call_end"} {
		t.Run(kind, func(t *testing.T) {
			f := taskPairFixture(t)
			f.write(f.meta(sessionA) + f.event("task_started", "a"))
			f.now = f.now.Add(time.Minute)
			f.append(f.event(kind, "a"))
			expectState(t, f.sample(), Working)
		})
	}
	for _, kind := range []string{"message", "reasoning", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output"} {
		t.Run("response "+kind, func(t *testing.T) {
			f := taskPairFixture(t)
			f.write(f.meta(sessionA) + f.event("task_started", "a"))
			f.now = f.now.Add(time.Minute)
			raw, _ := json.Marshal(map[string]any{"type": "response_item", "timestamp": f.now.Format(time.RFC3339Nano), "payload": map[string]any{"type": kind, "turn_id": "a", "role": "assistant"}})
			f.append(string(raw) + "\n")
			expectState(t, f.sample(), Working)
		})
	}
	t.Run("progress outside turn", func(t *testing.T) {
		f := taskPairFixture(t)
		f.write(f.meta(sessionA) + f.event("agent_message", "a"))
		expectState(t, f.sample(), Unknown)
	})
	t.Run("progress wrong turn", func(t *testing.T) {
		f := taskPairFixture(t)
		f.write(f.meta(sessionA) + f.event("task_started", "a") + f.event("agent_message", "b"))
		expectState(t, f.sample(), Unknown)
	})
	t.Run("mtime is not progress", func(t *testing.T) {
		f := taskPairFixture(t)
		f.write(f.meta(sessionA) + f.event("task_started", "a"))
		expectState(t, f.sample(), Working)
		f.now = f.now.Add(5 * time.Minute)
		_ = os.Chtimes(f.path, f.now, f.now)
		expectState(t, f.sample(), Unknown)
	})
}

func TestTaskPairApprovalPreservesEventOrdering(t *testing.T) {
	f := taskPairFixture(t)
	f.write(f.meta(sessionA) + f.event("task_started", "a"))
	expectState(t, f.sample(), Working)
	f.now = f.now.Add(2 * time.Minute)
	f.append(f.event("permission_request", "a"))
	expectState(t, f.sample(), Unknown)
	f.now = f.now.Add(-time.Minute)
	f.append(f.event("agent_message", "a"))
	expectState(t, f.sample(), Unknown)
}
