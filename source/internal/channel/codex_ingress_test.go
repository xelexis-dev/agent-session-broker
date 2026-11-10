package channel

import (
	"context"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"strings"
	"sync"
	"testing"
)

func TestCodexIngressIdleAndToolBoundary(t *testing.T) {
	var pushed []broker.Message
	r := newCodexIngress(func(_ context.Context, m broker.Message) error { pushed = append(pushed, m); return nil })
	ctx := context.Background()
	if err := r.deliver(ctx, broker.Message{Body: "idle"}); err != nil {
		t.Fatal(err)
	}
	if len(pushed) != 1 {
		t.Fatal("idle did not wake through queue")
	}
	writeCodexHook(r, "UserPromptSubmit")
	r.deliver(ctx, broker.Message{From: "peer", Body: "during"})
	out, err := writeCodexHook(r, "PostToolUse")
	if err != nil || !strings.Contains(out["hookSpecificOutput"].(map[string]any)["additionalContext"].(string), "during") {
		t.Fatalf("missing hook message: %v %v", out, err)
	}
	again, _ := writeCodexHook(r, "PostToolUse")
	if len(again) != 0 || len(pushed) != 1 {
		t.Fatal("duplicate delivery")
	}
	writeCodexHook(r, "Stop")
	r.deliver(ctx, broker.Message{Body: "idle again"})
	if len(pushed) != 2 {
		t.Fatal("Stop did not restore idle queue")
	}
}

func TestCodexIngressStopDrainsBeforeBecomingIdle(t *testing.T) {
	r := newCodexIngress(func(context.Context, broker.Message) error { return errors.New("unexpected queue") })
	writeCodexHook(r, "PreToolUse")
	r.deliver(context.Background(), broker.Message{Body: "last boundary"})
	out, err := writeCodexHook(r, "Stop")
	if err != nil || out["decision"] != "block" || !strings.Contains(out["reason"].(string), "last boundary") {
		t.Fatalf("lost Stop input: %v %v", out, err)
	}
	out, _ = writeCodexHook(r, "Stop")
	if len(out) != 0 {
		t.Fatal("empty Stop continued forever")
	}
}

func TestCodexIngressConcurrentStopDoesNotLoseOrDuplicate(t *testing.T) {
	for i := 0; i < 100; i++ {
		var pushed int
		r := newCodexIngress(func(context.Context, broker.Message) error { pushed++; return nil })
		writeCodexHook(r, "PreToolUse")
		var wg sync.WaitGroup
		wg.Add(2)
		var out map[string]any
		go func() { defer wg.Done(); r.deliver(context.Background(), broker.Message{Body: "race"}) }()
		go func() { defer wg.Done(); out, _ = writeCodexHook(r, "Stop") }()
		wg.Wait()
		received := pushed
		if out["decision"] == "block" {
			received++
		}
		if received != 1 {
			t.Fatalf("delivery count %d", received)
		}
	}
}

func TestCodexIngressBoundedAndQueueError(t *testing.T) {
	r := newCodexIngress(func(context.Context, broker.Message) error { return errors.New("queue unavailable") })
	if r.deliver(context.Background(), broker.Message{Body: "idle"}) == nil {
		t.Fatal("hidden queue error")
	}
	writeCodexHook(r, "PreToolUse")
	for i := 0; i < 32; i++ {
		if err := r.deliver(context.Background(), broker.Message{Body: "item"}); err != nil {
			t.Fatal(err)
		}
	}
	if r.deliver(context.Background(), broker.Message{Body: "overflow"}) == nil {
		t.Fatal("unbounded queue")
	}
}

// interrupt is the test shorthand for the binding-fenced Interrupt hook path.
func (r *codexIngress) interrupt(ctx context.Context) error {
	callbacks, err := r.interruptHandoff(ctx)
	return finishQueueHandoffs(callbacks, err)
}
