package localactivity

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Own completed conversation turn, 2026-10-10: selected event types/timestamps
// only, with synthetic turn IDs. No message content, paths or producer pin.
func TestCapturedProgressInheritanceReplay(t *testing.T) {
	replaySelectedTurn(t, "testdata/codex-progress-selected.jsonl")
}

func TestCapturedCompactionNeutralReplay(t *testing.T) {
	replaySelectedTurn(t, "testdata/codex-compaction-selected.jsonl")
}

func replaySelectedTurn(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	f := taskPairFixture(t)
	f.write(f.meta(sessionA))
	counts := map[string]int{}
	for i, line := range lines {
		var event struct {
			Timestamp time.Time `json:"timestamp"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		f.now = event.Timestamp
		f.append(string(line) + "\n")
		got := f.sample()
		counts[got.State]++
		want := Working
		if i == len(lines)-1 {
			want = Idle
		}
		if got.State != want {
			t.Fatalf("selected event %d: %s/%s; want %s", i, got.State, got.Reason, want)
		}
	}
	t.Logf("selected replay state counts: %v", counts)
}

func selectedEvent(f *fixture, envelope, kind, id string) string {
	payload := map[string]string{"type": kind}
	if id != "" {
		payload["turn_id"] = id
	}
	raw, _ := json.Marshal(map[string]any{"type": envelope, "timestamp": f.now.Format(time.RFC3339Nano), "payload": payload})
	return string(raw) + "\n"
}

func TestProgressInheritsOnlySingleOpenTurn(t *testing.T) {
	for _, kind := range []string{"message", "reasoning", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output"} {
		t.Run(kind, func(t *testing.T) {
			f := taskPairFixture(t)
			f.write(f.meta(sessionA) + f.event("task_started", "a"))
			f.now = f.now.Add(4 * time.Minute)
			f.append(selectedEvent(f, "response_item", kind, ""))
			expectState(t, f.sample(), Working)
			f.now = f.now.Add(4 * time.Minute)
			f.append(selectedEvent(f, "response_item", kind, ""))
			expectState(t, f.sample(), Working)
			f.append(f.event("task_complete", "a"))
			expectState(t, f.sample(), Idle)
		})
	}
	for _, tc := range []struct{ name, envelope, kind, id string }{
		{"overlapping start", "event_msg", "task_started", "b"},
		{"duplicate start", "event_msg", "task_started", "a"},
		{"progress mismatch", "response_item", "reasoning", "b"},
		{"bookkeeping mismatch", "turn_context", "", "b"},
		{"item mismatch", "event_msg", "item_completed", "b"},
		{"missing completion id", "event_msg", "task_complete", ""},
		{"event progress cannot inherit", "event_msg", "agent_message", ""},
		{"unknown activity", "response_item", "new_event", ""},
		{"abort", "event_msg", "turn_aborted", "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := taskPairFixture(t)
			f.write(f.meta(sessionA) + f.event("task_started", "a"))
			f.append(selectedEvent(f, tc.envelope, tc.kind, tc.id))
			expectState(t, f.sample(), Unknown)
			f.append(selectedEvent(f, "response_item", "reasoning", "") + f.event("task_complete", "a"))
			expectState(t, f.sample(), Unknown)
		})
	}
	t.Run("no start", func(t *testing.T) {
		f := taskPairFixture(t)
		f.write(f.meta(sessionA) + selectedEvent(f, "response_item", "reasoning", ""))
		expectState(t, f.sample(), Unknown)
	})
	t.Run("late inherited progress", func(t *testing.T) {
		f := taskPairFixture(t)
		f.write(f.meta(sessionA) + f.event("task_started", "a"))
		f.now = f.now.Add(5 * time.Minute)
		f.append(selectedEvent(f, "response_item", "function_call_output", ""))
		expectState(t, f.sample(), Working)
	})
	t.Run("backwards inherited progress", func(t *testing.T) {
		f := taskPairFixture(t)
		f.write(f.meta(sessionA) + f.event("task_started", "a"))
		f.now = f.now.Add(-time.Second)
		f.append(selectedEvent(f, "response_item", "reasoning", ""))
		expectState(t, f.sample(), Unknown)
	})
}

func TestBookkeepingAndItemCompletedAreNeutral(t *testing.T) {
	t.Run("compacted gap longer than five minutes", func(t *testing.T) {
		f := taskPairFixture(t)
		f.write(f.meta(sessionA) + f.event("task_started", "a"))
		expectState(t, f.sample(), Working)
		f.now = f.now.Add(6 * time.Minute)
		f.append(selectedEvent(f, "compacted", "", ""))
		got := f.sample()
		if got.State != Unknown || got.Reason != "stale-evidence" {
			t.Fatalf("compacted refreshed stale timer: %+v", got)
		}
	})
	for _, tc := range []struct{ envelope, kind string }{
		{"turn_context", ""}, {"token_usage_record", ""}, {"event_msg", "token_count"}, {"event_msg", "item_completed"},
		{"compacted", ""}, {"world_state", ""}, {"event_msg", "thread_settings_applied"},
	} {
		for _, id := range []string{"", "a"} {
			t.Run(tc.envelope+"/"+tc.kind+"/"+id, func(t *testing.T) {
				f := taskPairFixture(t)
				f.write(f.meta(sessionA) + f.event("task_started", "a"))
				expectState(t, f.sample(), Working)
				f.now = f.now.Add(4 * time.Minute)
				f.append(selectedEvent(f, tc.envelope, tc.kind, id))
				expectState(t, f.sample(), Working)
				f.now = f.now.Add(time.Minute)
				f.append(selectedEvent(f, tc.envelope, tc.kind, id))
				got := f.sample()
				if got.State != Unknown || got.Reason != "stale-evidence" {
					t.Fatalf("neutral extended timer: %+v", got)
				}
			})
		}
		t.Run(tc.envelope+"/"+tc.kind+" mismatched id", func(t *testing.T) {
			f := taskPairFixture(t)
			f.write(f.meta(sessionA) + f.event("task_started", "a"))
			f.append(selectedEvent(f, tc.envelope, tc.kind, "b"))
			expectState(t, f.sample(), Unknown)
			f.append(f.event("task_complete", "a"))
			expectState(t, f.sample(), Unknown)
		})
		t.Run(tc.envelope+"/"+tc.kind+" without start", func(t *testing.T) {
			f := taskPairFixture(t)
			f.write(f.meta(sessionA) + selectedEvent(f, tc.envelope, tc.kind, ""))
			expectState(t, f.sample(), Unknown)
		})
	}
	t.Run("approval remains unknown until progress", func(t *testing.T) {
		f := taskPairFixture(t)
		f.write(f.meta(sessionA) + f.event("task_started", "a"))
		f.append(f.event("permission_request", "a") + selectedEvent(f, "event_msg", "item_completed", "a"))
		expectState(t, f.sample(), Unknown)
		f.append(selectedEvent(f, "response_item", "reasoning", ""))
		expectState(t, f.sample(), Working)
		f.append(f.event("task_complete", "a"))
		expectState(t, f.sample(), Idle)
	})
}
