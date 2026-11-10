package localactivity

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Synthetic selected shapes only: a 5m27s silent gap followed by progress and
// the explicit matching completion. No rollout bodies or real identifiers.
func TestTaskPairStaleSameTurnRecovery(t *testing.T) {
	raw, err := os.ReadFile("testdata/codex-stale-recovery-selected.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	f := taskPairFixture(t)
	f.write(f.meta(sessionA))
	for i, line := range lines {
		var event struct{ Timestamp time.Time }
		if json.Unmarshal(line, &event) != nil {
			t.Fatal("invalid selected fixture")
		}
		f.now = event.Timestamp
		if i == 2 {
			got := f.sample()
			if got.State != Unknown || got.Reason != "stale-evidence" {
				t.Errorf("5m27s gap: %s/%s; want unknown/stale-evidence", got.State, got.Reason)
			}
		}
		f.append(string(line) + "\n")
		got := f.sample()
		want := Working
		if i == len(lines)-1 {
			want = Idle
		}
		if got.State != want || got.Reason != "" {
			t.Errorf("selected event %d: %s/%s; want %s", i, got.State, got.Reason, want)
		}
	}
}

func TestTaskPairStaleProgressVariants(t *testing.T) {
	for _, tc := range []struct{ envelope, kind, id string }{
		{"event_msg", "agent_message", "a"}, {"event_msg", "agent_reasoning", "a"},
		{"event_msg", "exec_command_begin", "a"}, {"event_msg", "exec_command_end", "a"},
		{"event_msg", "mcp_tool_call_begin", "a"}, {"event_msg", "mcp_tool_call_end", "a"},
		{"response_item", "message", ""}, {"response_item", "reasoning", ""},
		{"response_item", "function_call", ""}, {"response_item", "function_call_output", ""},
		{"response_item", "custom_tool_call", ""}, {"response_item", "custom_tool_call_output", ""},
		{"response_item", "custom_tool_call_output", "a"},
	} {
		t.Run(tc.envelope+"/"+tc.kind+"/"+tc.id, func(t *testing.T) {
			f := taskPairFixture(t)
			f.write(f.meta(sessionA) + f.event("task_started", "a"))
			expectState(t, f.sample(), Working)
			f.now = f.now.Add(5*time.Minute + 27*time.Second)
			for i := 0; i < 2; i++ {
				got := f.sample()
				if got.State != Unknown || got.Reason != "stale-evidence" {
					t.Fatal("silence must stay stale on reread")
				}
			}
			f.append(selectedEvent(f, tc.envelope, tc.kind, tc.id))
			expectState(t, f.sample(), Working)
			f.append(f.event("task_complete", "a"))
			expectState(t, f.sample(), Idle)
		})
	}
}

func TestTaskPairStaleCannotResurrectInvalidatedTurn(t *testing.T) {
	for _, tc := range []struct{ name, closing string }{
		{"completion", "task_complete"}, {"abort", "turn_aborted"}, {"overlapping newer start", "task_started"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := taskPairFixture(t)
			f.write(f.meta(sessionA) + f.event("task_started", "a"))
			expectState(t, f.sample(), Working)
			f.now = f.now.Add(5*time.Minute + 27*time.Second)
			expectState(t, f.sample(), Unknown)
			id := "a"
			if tc.closing == "task_started" {
				id = "b"
			}
			f.append(f.event(tc.closing, id))
			_ = f.sample()
			f.now = f.now.Add(time.Second)
			f.append(selectedEvent(f, "response_item", "custom_tool_call_output", "a"))
			expectState(t, f.sample(), Unknown)
			// Nor can an anonymous response item or matching old completion reopen it.
			f.append(selectedEvent(f, "response_item", "reasoning", "") + f.event("task_complete", "a"))
			expectState(t, f.sample(), Unknown)
			f.now = f.now.Add(time.Second)
			f.append(f.event("task_started", "fresh") + f.event("task_complete", "fresh"))
			expectState(t, f.sample(), Idle)
		})
	}
	t.Run("completed old turn while a new turn is open", func(t *testing.T) {
		f := taskPairFixture(t)
		f.write(f.meta(sessionA) + f.event("task_started", "a") + f.event("task_complete", "a") + f.event("task_started", "b"))
		expectState(t, f.sample(), Working)
		f.now = f.now.Add(5*time.Minute + 27*time.Second)
		expectState(t, f.sample(), Unknown)
		f.append(f.event("agent_message", "a"))
		expectState(t, f.sample(), Unknown)
	})
}

func TestTaskPairStaleStillRejectsMalformedProgress(t *testing.T) {
	for _, tc := range []struct {
		name, envelope, kind, id string
		shift                    time.Duration
	}{
		{"wrong turn", "response_item", "custom_tool_call", "b", 0},
		{"missing required id", "event_msg", "agent_message", "", 0},
		{"unknown shape", "response_item", "future_item", "a", 0},
		{"backwards", "event_msg", "agent_message", "a", -6 * time.Minute},
		{"future", "event_msg", "agent_message", "a", 2 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := taskPairFixture(t)
			f.write(f.meta(sessionA) + f.event("task_started", "a"))
			expectState(t, f.sample(), Working)
			f.now = f.now.Add(5*time.Minute + 27*time.Second)
			expectState(t, f.sample(), Unknown)
			now := f.now
			f.now = f.now.Add(tc.shift)
			line := selectedEvent(f, tc.envelope, tc.kind, tc.id)
			f.now = now
			f.append(line)
			got := f.sample()
			if got.State != Unknown || got.Reason != "malformed-evidence" {
				t.Fatal("malformed input must stay distinct from staleness")
			}
			f.append(f.event("agent_message", "a") + f.event("task_complete", "a"))
			expectState(t, f.sample(), Unknown)
		})
	}
}

func TestTaskPairStaleNeutralDoesNotRefreshAndCompletionCloses(t *testing.T) {
	f := taskPairFixture(t)
	f.write(f.meta(sessionA) + f.event("task_started", "a"))
	expectState(t, f.sample(), Working)
	f.now = f.now.Add(5*time.Minute + 27*time.Second)
	f.append(selectedEvent(f, "compacted", "", ""))
	got := f.sample()
	if got.State != Unknown || got.Reason != "stale-evidence" {
		t.Fatal("neutral event must not refresh stale evidence")
	}
	f.append(f.event("task_complete", "a"))
	expectState(t, f.sample(), Idle)
	f.observer = New(f.config)
	expectState(t, f.sample(), Idle) // Cold replay agrees with incremental observation.
}

func TestTaskPairStaleRetainsFileIntegrityFence(t *testing.T) {
	f := taskPairFixture(t)
	f.write(f.meta(sessionA) + f.event("task_started", "a"))
	expectState(t, f.sample(), Working)
	f.now = f.now.Add(5*time.Minute + 27*time.Second)
	expectState(t, f.sample(), Unknown)
	// Rewriting a verified prefix after it goes stale cannot turn it into fresh
	// same-turn progress. The prior file/anchor checks still apply.
	f.write(f.meta(sessionA) + f.event("task_started", "different") + f.event("agent_message", "different"))
	got := f.sample()
	if got.State != Unknown || got.Reason == "stale-evidence" || got.Reason == "" {
		t.Fatal("rewritten evidence bypassed integrity checks")
	}
	f.append(f.event("agent_message", "different"))
	expectState(t, f.sample(), Unknown)
}
