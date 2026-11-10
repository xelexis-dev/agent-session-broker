package localactivity

import (
	"testing"
	"time"
)

func TestTaskPair0164AdditionalToolsReplay(t *testing.T) {
	f := taskPairFixture(t)
	f.write(f.meta(sessionA) + f.event("task_started", "synthetic-a"))
	expectState(t, f.sample(), Working)
	type row struct{ envelope, kind, id string }
	rows := []row{{"response_item", "additional_tools", ""}}
	for range 5 {
		rows = append(rows, row{"response_item", "message", ""})
	}
	rows = append(rows, row{"world_state", "", ""}, row{"turn_context", "", ""},
		row{"response_item", "function_call", ""}, row{"event_msg", "task_complete", "synthetic-a"})
	for i, row := range rows {
		f.now = f.now.Add(time.Second)
		f.append(selectedEvent(f, row.envelope, row.kind, row.id))
		want := Working
		if i == len(rows)-1 {
			want = Idle
		}
		expectState(t, f.sample(), want)
	}
	f.observer = New(f.config)
	expectState(t, f.sample(), Idle)
}

func TestTaskPairCompletedNeutralPreservesCurrentTurn(t *testing.T) {
	for _, row := range []struct{ envelope, kind string }{
		{"event_msg", "item_completed"}, {"event_msg", "token_count"}, {"token_usage_record", ""},
	} {
		for _, shift := range []time.Duration{-time.Minute, 4 * time.Minute} {
			t.Run(row.envelope+"/"+row.kind+"/"+shift.String(), func(t *testing.T) {
				f := taskPairFixture(t)
				f.write(f.meta(sessionA) + f.event("task_started", "synthetic-a") + f.event("task_complete", "synthetic-a"))
				expectState(t, f.sample(), Idle)
				f.now = f.now.Add(2 * time.Minute)
				f.append(f.event("task_started", "synthetic-b") + f.event("agent_message", "synthetic-b"))
				expectState(t, f.sample(), Working)
				before := f.observer.rollout
				now := f.now
				f.now = f.now.Add(shift)
				line := selectedEvent(f, row.envelope, row.kind, "synthetic-a")
				f.now = now.Add(max(shift, 0))
				f.append(line)
				expectState(t, f.sample(), Working)
				after := f.observer.rollout
				if after.turn != before.turn || after.hasTurn != before.hasTurn || after.progress != before.progress || after.changed != before.changed || after.lastEvent != before.lastEvent || after.reason != before.reason {
					t.Fatal("ignored completed-turn bookkeeping changed current pair or clocks")
				}
				f.now = now.Add(5 * time.Minute)
				if got := f.sample(); got.State != Unknown || got.Reason != "stale-evidence" {
					t.Fatal("old neutral row refreshed current progress")
				}
				f.append(f.event("agent_message", "synthetic-b"))
				expectState(t, f.sample(), Working)
				f.append(f.event("task_complete", "synthetic-b"))
				expectState(t, f.sample(), Idle)
				f.observer = New(f.config)
				expectState(t, f.sample(), Idle)
			})
		}
	}
}

func TestTaskPair0164NeutralExceptionsStayNarrow(t *testing.T) {
	for _, row := range []struct{ name, envelope, kind, id string }{
		{"old progress", "event_msg", "agent_message", "synthetic-a"},
		{"old response progress", "response_item", "message", "synthetic-a"},
		{"old approval", "event_msg", "permission_request", "synthetic-a"},
		{"uncompleted neutral", "event_msg", "item_completed", "unseen-turn"},
		{"old world state", "world_state", "", "synthetic-a"},
		{"old settings", "event_msg", "thread_settings_applied", "synthetic-a"},
		{"old additional tools", "response_item", "additional_tools", "synthetic-a"},
		{"unknown", "response_item", "future_kind", "synthetic-a"},
		{"abort", "event_msg", "turn_aborted", "synthetic-b"},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := taskPairFixture(t)
			f.write(f.meta(sessionA) + f.event("task_started", "synthetic-a") + f.event("task_complete", "synthetic-a") + f.event("task_started", "synthetic-b"))
			expectState(t, f.sample(), Working)
			f.now = f.now.Add(time.Second)
			f.append(selectedEvent(f, row.envelope, row.kind, row.id))
			if got := f.sample(); got.State != Unknown || got.Reason != "malformed-evidence" {
				t.Fatal("exception accepted unproven event or turn")
			}
		})
	}
	t.Run("additional tools needs an open pair", func(t *testing.T) {
		f := taskPairFixture(t)
		f.write(f.meta(sessionA) + selectedEvent(f, "response_item", "additional_tools", ""))
		expectState(t, f.sample(), Unknown)
	})
	t.Run("additional tools does not extend progress", func(t *testing.T) {
		f := taskPairFixture(t)
		f.write(f.meta(sessionA) + f.event("task_started", "synthetic-a"))
		expectState(t, f.sample(), Working)
		f.now = f.now.Add(6 * time.Minute)
		f.append(selectedEvent(f, "response_item", "additional_tools", ""))
		if got := f.sample(); got.State != Unknown || got.Reason != "stale-evidence" {
			t.Fatal("additional_tools refreshed stale pair")
		}
		f.append(f.event("agent_message", "synthetic-a") + f.event("task_complete", "synthetic-a"))
		expectState(t, f.sample(), Idle)
	})
	t.Run("only the last completed turn is remembered", func(t *testing.T) {
		f := taskPairFixture(t)
		f.write(f.meta(sessionA) + f.event("task_started", "synthetic-a") + f.event("task_complete", "synthetic-a") + f.event("task_started", "synthetic-b") + f.event("task_complete", "synthetic-b"))
		expectState(t, f.sample(), Idle)
		f.append(f.event("item_completed", "synthetic-a"))
		expectState(t, f.sample(), Unknown)
	})
	t.Run("ignored neutral still validates time", func(t *testing.T) {
		f := taskPairFixture(t)
		f.write(f.meta(sessionA) + f.event("task_started", "synthetic-a") + f.event("task_complete", "synthetic-a"))
		expectState(t, f.sample(), Idle)
		now := f.now
		f.now = f.now.Add(2 * time.Minute)
		line := f.event("item_completed", "synthetic-a")
		f.now = now
		f.append(line)
		expectState(t, f.sample(), Unknown)
	})
}
