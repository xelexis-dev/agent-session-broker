package channel

import (
	"encoding/json"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"strings"
	"testing"
)

func TestKimiTurnBoundaryQueue(t *testing.T) {
	r := &kimiIngress{}
	enqueue := func(body string) {
		t.Helper()
		if err := r.deliver(t.Context(), broker.Message{From: "peer", To: "kimi", Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	enqueue("first")
	for _, event := range []string{"SessionStart", "PreToolUse", "PostToolUse", "StopFailure", "Interrupt"} {
		out, drained := r.hook(event)
		if drained || len(out) != 0 || r.pendingCount() != 1 {
			t.Fatal("non-delivery hook consumed queue", event)
		}
	}
	out, drained := r.hook("UserPromptSubmit")
	if !drained || !strings.Contains(out["message"].(string), "first") {
		t.Fatal(out)
	}
	enqueue("stop")
	out, drained = r.hook("Stop")
	if !drained {
		t.Fatal("Stop not drained")
	}
	v := out["hookSpecificOutput"].(map[string]any)
	if v["permissionDecision"] != "deny" || !strings.Contains(v["permissionDecisionReason"].(string), "stop") {
		t.Fatal(out)
	}
	enqueue("after-continuation")
	out, drained = r.hook("Stop")
	if drained || len(out) != 0 || r.pendingCount() != 1 {
		t.Fatal("second Stop drained")
	}
	out, drained = r.hook("UserPromptSubmit")
	if !drained || !strings.Contains(out["message"].(string), "after-continuation") {
		t.Fatal("next prompt lost pending")
	}
	r.fence()
	if r.deliver(t.Context(), broker.Message{}) == nil {
		t.Fatal("fenced queue accepted")
	}
}
func TestKimiQueueLimits(t *testing.T) {
	r := &kimiIngress{}
	for i := 0; i < 32; i++ {
		if e := r.deliver(t.Context(), broker.Message{}); e != nil {
			t.Fatal(e)
		}
	}
	if r.deliver(t.Context(), broker.Message{}) == nil {
		t.Fatal("count limit")
	}
	r = &kimiIngress{}
	m := broker.Message{Body: strings.Repeat("x", 11000)}
	for i := 0; i < 5; i++ {
		if e := r.deliver(t.Context(), m); e != nil {
			t.Fatal(e)
		}
	}
	if r.deliver(t.Context(), m) == nil {
		t.Fatal("byte limit")
	}
	if r.deliver(t.Context(), broker.Message{Body: strings.Repeat("x", 12001)}) == nil {
		t.Fatal("envelope limit")
	}
	out, _ := r.hook("UserPromptSubmit")
	data, _ := json.Marshal(out)
	if !json.Valid(data) {
		t.Fatal("invalid hook output")
	}
}

func (r *kimiIngress) hook(event string) (map[string]any, bool) {
	out, drained, _ := r.hookHandoff(event, false)
	return out, drained
}
