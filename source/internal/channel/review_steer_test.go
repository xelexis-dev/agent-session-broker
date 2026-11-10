package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestReviewSteerAutomaticEpochRecovery(t *testing.T) {
	up := newSwitchableUpstream(t)
	store, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := (durableOptions{enabled: true, state: store, scope: "http://broker/mcp?project=brk", project: "brk", heartbeat: time.Hour, timeout: time.Second, hookWrites: &hookWriteCompletions{}, backoff: func(int) time.Duration { return time.Millisecond }}).withDefaults()
	id := broker.AttachRequest{Harness: "codex", Name: "epoch-client", SessionID: "11111111-1111-4111-8111-111111111111", PID: 4242, CWD: "/synthetic", ChannelID: "first-lease"}
	b, unlock := ackClientBinding(t, opts, id)
	defer unlock()
	registered, err := registerDurable(t.Context(), up, b, opts, id)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSteerControl{result: context.DeadlineExceeded}
	r, _, _ := steerFixture(t, f)
	b.router = r
	var oldACK atomic.Int32
	old := context.WithValue(t.Context(), handoffKey{}, handoffCompletion{registration: registered.RegistrationID, epoch: registered.Epoch, done: func() error { oldACK.Add(1); return nil }})
	if err := r.deliver(old, broker.Message{RegistrationID: registered.RegistrationID, DeliverySequence: 1, Body: "old-epoch"}); err != nil {
		t.Fatal(err)
	}
	up.restart()
	if !recoverDurable(t.Context(), up, b, opts) {
		t.Fatal("automatic recovery failed")
	}
	if bindingMessageAck(b).epoch == registered.Epoch {
		t.Fatal("broker epoch unchanged")
	}
	f.mu.Lock()
	f.result = nil
	f.found = true
	f.mu.Unlock()
	delivered := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		receiveDurableOnce(ctx, up, b, opts, func(ctx context.Context, m broker.Message) error {
			meta := handoff(ctx)
			complete := meta.done
			meta.done = func() error {
				err := complete()
				if err == nil {
					delivered <- struct{}{}
				}
				return err
			}
			return r.deliver(context.WithValue(ctx, handoffKey{}, meta), m)
		})
	}()
	defer func() { cancel(); <-done }()
	var sent broker.SendResult
	if err := call(t.Context(), up, "send_message", broker.Message{From: registered.Name, To: registered.Name, Body: "new-epoch"}, &sent); err != nil || !sent.Accepted {
		t.Fatal("send new epoch", err)
	}
	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("new epoch blocked by old uncertain steer")
	}
	if oldACK.Load() != 0 {
		t.Fatal("old epoch acknowledged")
	}
}

func TestReviewSteerCloseResetsGrace(t *testing.T) {
	f := &fakeSteerControl{generationPhase: true}
	r, _, completed := steerFixture(t, f)
	r.steer.delay = time.Hour
	if err := steerMessage(r, completed, 1); err != nil {
		t.Fatal(err)
	}
	r.close()
	r.startReceiver()
	r.mu.Lock()
	stale := !r.steer.due.IsZero() || r.steer.timer != nil
	r.mu.Unlock()
	if stale {
		t.Fatal("stopped timer retained its due time")
	}
	r.nextGeneration()
	if err := steerMessage(r, completed, 2); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.steer.timer == nil || !r.steer.due.After(time.Now()) {
		t.Fatal("new generation did not arm a fresh grace timer")
	}
}

type reviewHookWire struct{ ackWriteProbe }

func (reviewHookWire) Close() error { return nil }
func TestReviewHookFailureReleasesFIFO(t *testing.T) {
	for _, mode := range []string{"isError", "rpc-error", "write-error", "close"} {
		t.Run(mode, func(t *testing.T) {
			r := newCodexIngress(func(context.Context, broker.Message) error { return nil })
			defer r.close()
			writeCodexHook(r, "UserPromptSubmit", "T")
			var ack int
			ctx := context.WithValue(t.Context(), handoffKey{}, handoffCompletion{done: func() error { ack++; return nil }})
			if err := r.deliver(ctx, broker.Message{DeliverySequence: 1, Body: "first"}); err != nil {
				t.Fatal(err)
			}
			out, complete, failed, err := r.hookHandoff("PostToolUse", "T")
			if err != nil || complete == nil {
				t.Fatal("handoff missing")
			}
			if err := r.deliver(ctx, broker.Message{DeliverySequence: 2, Body: "second"}); err != nil {
				t.Fatal(err)
			}
			writes := &hookWriteCompletions{}
			result := writes.result(complete, failed)
			result.StructuredContent = out
			result.IsError = mode == "isError"
			raw, _ := json.Marshal(result)
			wire := &hookWriteConnection{completions: writes, Connection: reviewHookWire{ackWriteProbe{write: func(context.Context, jsonrpc.Message) error {
				if mode == "write-error" {
					return io.ErrClosedPipe
				}
				return nil
			}}}}
			if mode == "close" {
				_ = wire.Close()
			} else {
				response := &jsonrpc.Response{Result: raw}
				if mode == "rpc-error" {
					response.Error = errors.New("fixture response error")
				}
				_ = wire.Write(t.Context(), response)
			}
			if ack != 0 {
				t.Fatal("failed hook acknowledged")
			}
			next, done, _, err := r.hookHandoff("PostToolUse", "T")
			if err != nil || len(next) == 0 || done == nil {
				t.Fatal("failed hook stuck or lost batch")
			}
			text := fmt.Sprint(next)
			if strings.Index(text, "first") >= strings.Index(text, "second") {
				t.Fatal("failed batch did not return to FIFO front")
			}
			if err := done(); err != nil {
				t.Fatal(err)
			}
			if ack != 2 {
				t.Fatal("retry did not ACK exactly once")
			}
		})
	}
}

// Test hooks use the same result marker and transport completion as the host.
func writeCodexHook(r *codexIngress, event string, turns ...string) (map[string]any, error) {
	out, done, failed, err := r.hookHandoff(event, turns...)
	if err != nil || done == nil {
		return out, err
	}
	writes := &hookWriteCompletions{}
	result := writes.result(done, failed)
	result.StructuredContent = out
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	wire := hookWriteConnection{completions: writes, Connection: reviewHookWire{ackWriteProbe{write: func(context.Context, jsonrpc.Message) error { return nil }}}}
	return out, wire.Write(context.Background(), &jsonrpc.Response{Result: raw})
}

func TestReviewSteerRetiredControlCompletion(t *testing.T) {
	for _, reply := range []error{nil, context.DeadlineExceeded, &steerRPCError{-32601, "unsupported"}} {
		t.Run(fmt.Sprint(reply), func(t *testing.T) {
			f := &fakeSteerControl{result: reply, entered: make(chan struct{}, 1), release: make(chan struct{})}
			r, pushed, completed := steerFixture(t, f)
			done := make(chan error, 1)
			go func() { done <- steerMessage(r, completed, 1) }()
			<-f.entered
			if !r.bindRegistration("R", "E2") {
				t.Fatal("batch not retired")
			}
			var newACK int
			ctx := context.WithValue(t.Context(), handoffKey{}, handoffCompletion{registration: "R", epoch: "E2", done: func() error { newACK++; return nil }})
			if err := r.deliver(ctx, broker.Message{RegistrationID: "R", DeliverySequence: 1, Body: "new"}); err != nil {
				t.Fatal(err)
			}
			// The next sink is the hook; no old control reply may complete its callback.
			out, hookDone, _, err := r.hookHandoff("PostToolUse", "T")
			if err != nil || len(out) == 0 || hookDone == nil {
				t.Fatal("new batch did not reach hook")
			}
			close(f.release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if newACK != 0 || *completed != 0 || *pushed != 0 {
				t.Fatal("retired control ACKed replacement")
			}
			if r.owner != "hook-writing" || r.steer.disabled {
				t.Fatal("retired control changed new owner/gate")
			}
			if err := hookDone(); err != nil || newACK != 1 {
				t.Fatal("new handoff did not complete once")
			}
		})
	}
}
