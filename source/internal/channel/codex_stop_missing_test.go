package channel

import (
	"context"
	"errors"
	"sync"
	"testing"

	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestCodexStopMissingDrainsOnceInOrder(t *testing.T) {
	var pushed []string
	ended := false
	r := newCodexIngress(func(_ context.Context, m broker.Message) error { pushed = append(pushed, m.Body); return nil })
	r.probe = func(context.Context, string) bool { return ended }
	writeCodexHook(r, "UserPromptSubmit", "a")
	writeCodexHook(r, "PostToolUse", "a")
	for _, body := range []string{"one", "two"} {
		if err := r.deliver(t.Context(), broker.Message{Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	ended = true
	if err := r.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(pushed) != 2 || pushed[0] != "one" || pushed[1] != "two" {
		t.Fatalf("pushes=%v want ordered pair once", pushed)
	}
}
func TestCodexStopMissingHookFencesProbe(t *testing.T) {
	for _, hook := range []string{"PostToolUse", "UserPromptSubmit", "Stop"} {
		t.Run(hook, func(t *testing.T) {
			pushed := 0
			r := newCodexIngress(func(context.Context, broker.Message) error { pushed++; return nil })
			writeCodexHook(r, "UserPromptSubmit", "a")
			r.deliver(t.Context(), broker.Message{Body: "pending"})
			r.probe = func(context.Context, string) bool { writeCodexHook(r, hook, "b"); return true }
			if err := r.flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if pushed != 0 {
				t.Fatal("probe crossed hook boundary")
			}
		})
	}
}
func TestCodexStopMissingMissingTurnKeepsPending(t *testing.T) {
	pushed := 0
	r := newCodexIngress(func(context.Context, broker.Message) error { pushed++; return nil })
	r.probe = func(context.Context, string) bool { return true }
	writeCodexHook(r, "UserPromptSubmit")
	r.deliver(t.Context(), broker.Message{Body: "old config"})
	r.flush(t.Context())
	if pushed != 0 || len(r.pending) != 1 {
		t.Fatal("missing turn ID was guessed")
	}
}
func TestCodexStopAndMissingStopRaceOwnMessageOnce(t *testing.T) {
	for range 50 {
		pushed := 0
		r := newCodexIngress(func(context.Context, broker.Message) error { pushed++; return nil })
		writeCodexHook(r, "UserPromptSubmit", "a")
		r.deliver(t.Context(), broker.Message{Body: "pending"})
		r.probe = func(context.Context, string) bool { return true }
		var out map[string]any
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); r.flush(t.Context()) }()
		go func() { defer wg.Done(); out, _ = writeCodexHook(r, "Stop", "a") }()
		wg.Wait()
		count := pushed
		if out["decision"] == "block" {
			count++
		}
		if count != 1 {
			t.Fatalf("delivered %d times", count)
		}
	}
}

func TestCodexStopMissingReplacedBindingKeepsPending(t *testing.T) {
	for _, mode := range []string{"generation", "closed"} {
		t.Run(mode, func(t *testing.T) {
			pushed := 0
			r := newCodexIngress(func(context.Context, broker.Message) error { pushed++; return nil })
			writeCodexHook(r, "UserPromptSubmit", "a")
			r.deliver(t.Context(), broker.Message{Body: "old binding"})
			r.probe = func(context.Context, string) bool {
				if mode == "generation" {
					r.nextGeneration()
				} else {
					r.mu.Lock()
					r.closed = true
					r.mu.Unlock()
				}
				return true
			}
			r.flush(t.Context())
			if pushed != 0 || len(r.pending) != 1 {
				t.Fatal("probe crossed binding replacement")
			}
		})
	}
}

func TestCodexStopMissingCloseJoinsProbe(t *testing.T) {
	started, release, done, closed := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	r := newCodexIngress(func(context.Context, broker.Message) error { t.Error("closed router pushed"); return nil })
	writeCodexHook(r, "UserPromptSubmit", "a")
	r.deliver(t.Context(), broker.Message{Body: "pending"})
	r.probe = func(context.Context, string) bool { close(started); <-release; return true }
	go func() { defer close(done); r.flush(t.Context()) }()
	<-started
	go func() { r.close(); close(closed) }()
	waitFor(t, "closed flag", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.closed })
	select {
	case <-closed:
		t.Fatal("close failed to join probe")
	default:
	}
	close(release)
	<-done
	<-closed
	if len(r.pending) != 1 {
		t.Fatal("close lost pending ownership")
	}
}

func TestCodexStopMissingPushFailureRetainsRemainder(t *testing.T) {
	fail := true
	var pushed []string
	r := newCodexIngress(func(_ context.Context, m broker.Message) error {
		if m.Body == "two" && fail {
			return errors.New("queue failed")
		}
		pushed = append(pushed, m.Body)
		return nil
	})
	writeCodexHook(r, "UserPromptSubmit", "a")
	for _, body := range []string{"one", "two"} {
		r.deliver(t.Context(), broker.Message{Body: body})
	}
	r.probe = func(context.Context, string) bool { return true }
	recovered := 0
	r.onRecovery = func() { recovered++ }
	if r.flush(t.Context()) == nil {
		t.Fatal("push failure swallowed")
	}
	if len(pushed) != 1 || len(r.pending) != 1 || r.pending[0].Body != "two" || r.bytes == 0 {
		t.Fatal("failed push lost or duplicated ownership")
	}
	fail = false
	if err := r.interrupt(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(pushed) != 2 || pushed[1] != "two" || r.bytes != 0 || recovered != 1 {
		t.Fatal("interrupt retry changed accounting")
	}
}

func TestCodexStopMissingDeliverAlsoProbes(t *testing.T) {
	pushed := 0
	r := newCodexIngress(func(context.Context, broker.Message) error { pushed++; return nil })
	r.probe = func(context.Context, string) bool { return true }
	writeCodexHook(r, "UserPromptSubmit", "a")
	if err := r.deliver(t.Context(), broker.Message{Body: "first after failed turn"}); err != nil {
		t.Fatal(err)
	}
	if pushed != 1 || len(r.pending) != 0 {
		t.Fatal("deliver did not recover missed Stop")
	}
}
