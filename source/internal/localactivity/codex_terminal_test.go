package localactivity

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCodexTurnEndedRequiresLatestStartPair(t *testing.T) {
	for _, tc := range []struct {
		name, events string
		want         bool
	}{
		{"complete", "start:a,end:a", true},
		{"aborted", "start:a,abort:a", true},
		{"no terminal", "start:a", false},
		{"terminal without start", "end:a", false},
		{"B start before delayed A terminal", "start:a,start:b,end:a", false},
		{"B terminal cannot close A", "start:a,end:b", false},
		{"new start after A terminal", "start:a,end:a,start:b", false},
		{"missing start id", "start:,end:a", false},
		{"missing terminal id", "start:a,end:", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "codex")
			data := f.meta(sessionA)
			for _, item := range strings.Split(tc.events, ",") {
				parts := strings.SplitN(item, ":", 2)
				kind := map[string]string{"start": "task_started", "end": "task_complete", "abort": "turn_aborted"}[parts[0]]
				data += f.event(kind, parts[1])
			}
			f.write(data)
			if got := CodexTurnEnded(t.Context(), f.config, "a"); got != tc.want {
				t.Fatalf("terminal=%v want=%v", got, tc.want)
			}
			f.now = f.now.Add(-24 * time.Hour)
			if got := CodexTurnEnded(t.Context(), f.config, "a"); got != tc.want {
				t.Fatal("wall clock changed evidence")
			}
			if CodexTurnEnded(t.Context(), f.config, "") {
				t.Fatal("missing hook ID accepted")
			}
		})
	}
}
func TestCodexTurnEndedRejectsIncompleteTailBudgetAndOtherMeta(t *testing.T) {
	for _, kind := range []string{"partial", "outside budget", "wrong meta", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, "codex")
			data := f.meta(sessionA) + f.event("task_started", "a")
			ctx := t.Context()
			switch kind {
			case "partial":
				data += f.event("task_complete", "a") + `{"type":`
			case "outside budget":
				data += strings.Repeat("{}\n", (maxRead/3)+1) + f.event("task_complete", "a")
			case "wrong meta":
				data = f.meta(sessionB) + f.event("task_started", "a") + f.event("task_complete", "a")
			case "cancelled":
				data += f.event("task_complete", "a")
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			f.write(data)
			if CodexTurnEnded(ctx, f.config, "a") {
				t.Fatal("unsafe evidence accepted")
			}
		})
	}
}

func TestCodexTurnEndedRejectsReadTimeFileChange(t *testing.T) {
	for _, kind := range []string{"replace", "append", "truncate"} {
		t.Run(kind, func(t *testing.T) {
			if runtime.GOOS == "windows" && kind == "replace" {
				t.Skip("Windows refuses test-side replacement of an open evidence handle")
			}
			f := newFixture(t, "codex")
			data := f.meta(sessionA) + f.event("task_started", "a") + f.event("task_complete", "a")
			f.write(data)
			calls := 0
			f.config.Probe = func(context.Context, int) (Process, error) {
				calls++
				if calls == 2 {
					switch kind {
					case "replace":
						if err := os.WriteFile(f.path+"-new", []byte(data), 0600); err != nil {
							t.Fatal(err)
						}
						if err := os.Rename(f.path+"-new", f.path); err != nil {
							t.Fatal(err)
						}
					case "append":
						f.append("{}\n")
					case "truncate":
						f.write(f.meta(sessionA))
					}
				}
				return f.process, nil
			}
			if CodexTurnEnded(t.Context(), f.config, "a") {
				t.Fatal("changed file accepted")
			}
		})
	}
}

func TestCodexTurnEndedIgnoresTimestampOrder(t *testing.T) {
	f := newFixture(t, "codex")
	data := f.meta(sessionA) + f.event("task_started", "a")
	f.now = f.now.Add(-24 * time.Hour)
	f.write(data + f.event("task_complete", "a"))
	if !CodexTurnEnded(t.Context(), f.config, "a") {
		t.Fatal("clock inversion invalidated explicit pair")
	}
}

func TestCodexTurnEndedLatestHookTurnAndDelayedOlderTerminal(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprint(completed), func(t *testing.T) {
			f := newFixture(t, "codex")
			data := f.meta(sessionA) + f.event("task_started", "a") + f.event("task_started", "b")
			if completed {
				data += f.event("task_complete", "b")
			}
			data += f.event("task_complete", "a")
			f.write(data)
			if got := CodexTurnEnded(t.Context(), f.config, "b"); got != completed {
				t.Fatalf("B terminal=%v want=%v", got, completed)
			}
		})
	}
}
