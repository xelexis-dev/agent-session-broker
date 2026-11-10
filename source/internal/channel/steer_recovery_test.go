package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/codexcontrol"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

const recoveryThread = "11111111-1111-4111-8111-111111111111"

type steerRecoveryFixture struct {
	r                                           *codexIngress
	state                                       *messageAckState
	binding                                     *ingressBinding
	store                                       *clientstate.Store
	record                                      clientstate.SteerAttemptRecord
	visible, terminal, queueMatch, queueUnknown bool
	calls, scans                                int
	pushed                                      []uint64
	dialError                                   error
}

func newRecoveryFixture(t *testing.T, pending bool) *steerRecoveryFixture {
	t.Helper()
	if !codexControlAvailable {
		t.Skip("daemon steer recovery is not installed on this platform")
	}
	s, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &steerRecoveryFixture{store: s}
	f.state = &messageAckState{store: s, scope: "scope", registration: "R", epoch: "E", lease: "L", completed: map[uint64]bool{}, wake: make(chan struct{}, 1), failed: make(chan struct{})}
	f.binding = &ingressBinding{mu: &sync.Mutex{}, identity: broker.AttachRequest{ChannelID: "L"}, messageAck: f.state}
	f.record, err = s.LoadSteerAttempt("scope", recoveryThread)
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		next := f.record
		next.Registration = "R"
		next.Epoch = "E"
		next.Pending = scanRecord().Pending
		if err = s.SaveSteerAttempt("scope", recoveryThread, f.record, next); err != nil {
			t.Fatal(err)
		}
		f.record = next
	}
	f.r = newCodexIngress(func(_ context.Context, m broker.Message) error {
		f.pushed = append(f.pushed, m.DeliverySequence)
		return nil
	})
	f.r.steer = &codexSteerer{thread: recoveryThread, gate: func() string { return "off" }, dial: func(context.Context) (codexControl, error) {
		if f.dialError != nil {
			return nil, f.dialError
		}
		return scanControl{call: f.call}, nil
	}}
	f.r.recovery = &steerRecovery{store: s, scope: "scope", thread: recoveryThread, registration: "R", epoch: "E"}
	f.r.initSteerRecovery()
	t.Cleanup(f.r.close)
	return f
}
func (f *steerRecoveryFixture) call(_ context.Context, method string, _ any) (json.RawMessage, error) {
	f.calls++
	switch method {
	case "thread/turns/list":
		status := "inProgress"
		if f.terminal {
			status = "completed"
		}
		return json.Marshal(map[string]any{"data": []any{map[string]any{"id": "T", "status": status}}, "nextCursor": nil})
	case "thread/items/list":
		f.scans++
		if f.visible {
			return scanPage(scanMessages(), nil), nil
		}
		return scanPage(nil, nil), nil
	case "thread/queue/list":
		entries := []any{}
		if f.queueMatch {
			entries = append(entries, map[string]any{"clientUserMessageId": "xb:R:E:1-2", "input": []any{map[string]any{"type": "text", "text": "fixture"}}})
		}
		if f.queueUnknown {
			entries = append(entries, map[string]any{"id": "unknown"})
		}
		return json.Marshal(map[string]any{"data": entries, "nextCursor": nil})
	}
	return nil, errors.New("unexpected fixture method")
}
func (f *steerRecoveryFixture) deliver(t *testing.T, n int) {
	t.Helper()
	m := scanMessages()[n-1]
	m.RegistrationID = f.state.registration
	if _, err := f.state.observe(m.DeliverySequence); err != nil {
		t.Fatal(err)
	}
	meta := f.state.completion(t.Context(), f.binding, m.DeliverySequence)
	if err := f.r.deliver(context.WithValue(t.Context(), handoffKey{}, meta), m); err != nil {
		t.Fatal(err)
	}
}
func TestSteerRecoveryPositiveBeforeIdlePushAndPrefix(t *testing.T) {
	f := newRecoveryFixture(t, true)
	f.visible = true
	// The wire issue order stays FIFO; only the first handoff callback arrives late.
	if _, err := f.state.observe(1); err != nil {
		t.Fatal(err)
	}
	f.deliver(t, 2)
	if through, _ := f.state.snapshot(); through != 0 || len(f.pushed) != 0 {
		t.Fatal("late prefix member acknowledged/delivered early")
	}
	f.deliver(t, 1)
	if through, _ := f.state.snapshot(); through != 2 || len(f.pushed) != 0 {
		t.Fatal("positive replay reached a sink or failed prefix ACK")
	}
	record, err := f.store.LoadSteerAttempt("scope", recoveryThread)
	if err != nil || record.Pending != nil {
		t.Fatal("completed attempt not emptied")
	}
}
func TestSteerRecoveryAbsentFastPath(t *testing.T) {
	f := newRecoveryFixture(t, false)
	f.deliver(t, 1)
	if len(f.pushed) != 1 || f.calls != 0 {
		t.Fatal("unattempted message scanned or blocked")
	}
}
func TestSteerRecoveryActiveAndQueueHold(t *testing.T) {
	for _, mode := range []string{"active", "matching-queue", "unmatchable-queue"} {
		t.Run(mode, func(t *testing.T) {
			f := newRecoveryFixture(t, true)
			f.terminal = mode != "active"
			f.queueMatch = mode == "matching-queue"
			f.queueUnknown = mode == "unmatchable-queue"
			f.deliver(t, 1)
			f.deliver(t, 2)
			out, _, _, err := f.r.hookHandoff("Stop", "T")
			if err != nil || len(out) != 0 {
				t.Fatal("held replay escaped Stop")
			}
			_ = f.r.interrupt(t.Context())
			_ = f.r.flush(t.Context())
			if through, _ := f.state.snapshot(); through != 0 || len(f.pushed) != 0 {
				t.Fatal("absence/queue bypassed hold")
			}
			f.visible = true
			_ = f.r.flush(t.Context())
			if through, _ := f.state.snapshot(); through != 2 || len(f.pushed) != 0 {
				t.Fatal("positive evidence failed after hold")
			}
		})
	}
}
func TestSteerRecoveryInterruptAbsenceReleasesOnce(t *testing.T) {
	f := newRecoveryFixture(t, true)
	f.terminal = true
	f.deliver(t, 1)
	f.deliver(t, 2)
	if len(f.pushed) != 2 || f.pushed[0] != 1 || f.pushed[1] != 2 {
		t.Fatal("terminal fresh empty history/queue did not release FIFO once")
	}
	if through, _ := f.state.snapshot(); through != 2 {
		t.Fatal("released delivery not acknowledged")
	}
}
func TestSteerRecoveryDialFailureHoldsOnlyTypedModeSkips(t *testing.T) {
	for _, typed := range []bool{false, true} {
		t.Run(map[bool]string{false: "transient", true: "not-daemon"}[typed], func(t *testing.T) {
			f := newRecoveryFixture(t, true)
			f.dialError = errors.New("fixture unavailable")
			if typed {
				f.dialError = codexcontrol.ErrNotDaemonHost
			}
			f.deliver(t, 1)
			if typed && len(f.pushed) != 1 || !typed && len(f.pushed) != 0 {
				t.Fatal("dial failure misclassified as a mode switch")
			}
		})
	}
}
func TestSteerRecoveryCorruptNeverOpensAnySink(t *testing.T) {
	f := newRecoveryFixture(t, true)
	files, _ := filepath.Glob(filepath.Join(f.store.Dir(), "steer-attempts", "v1", "*.json"))
	if len(files) != 1 {
		t.Fatal("fixture record")
	}
	raw := []byte(`{"format":9}`)
	if os.WriteFile(files[0], raw, 0600) != nil {
		t.Fatal("fixture corrupt")
	}
	f.r.mu.Lock()
	f.r.recovery.loaded = false
	f.r.mu.Unlock()
	f.r.initSteerRecovery()
	f.deliver(t, 1)
	_ = f.r.interrupt(t.Context())
	_ = f.r.flush(t.Context())
	out, _, _, _ := f.r.hookHandoff("Stop", "T")
	after, _ := os.ReadFile(files[0])
	if len(out) != 0 || len(f.pushed) != 0 || string(after) != string(raw) {
		t.Fatal("corruption was released or overwritten")
	}
	if f.r.deliveryStatus().Reason != "steer-recovery-unavailable" {
		t.Fatal("unavailable reason missing")
	}
}
func TestSteerRecoveryEpochRetiresWithoutOldACK(t *testing.T) {
	f := newRecoveryFixture(t, true)
	f.deliver(t, 1)
	f.r.bindRegistration("R", "E2")
	f.r.nextGeneration()
	f.r.initSteerRecovery()
	old := f.state
	f.state = &messageAckState{store: f.store, scope: "scope", registration: "R", epoch: "E2", lease: "L2", completed: map[uint64]bool{}, wake: make(chan struct{}, 1), failed: make(chan struct{})}
	f.binding.messageAck = f.state
	f.binding.identity.ChannelID = "L2"
	f.deliver(t, 1)
	if old.through != 0 || len(f.pushed) != 1 {
		t.Fatal("old epoch ACKed or new epoch blocked")
	}
}

func TestSteerRecoveryRequiresConfirmedThreadEvenWhenOff(t *testing.T) {
	f := newRecoveryFixture(t, true)
	f.visible = true
	f.r.steer.confirmed = func() bool { return false }
	f.deliver(t, 1)
	if f.calls != 0 || len(f.pushed) != 0 {
		t.Fatal("unconfirmed identity read history or opened sink")
	}
}
func TestSteerRecoveryRouterReplacementUsesNewCallbacks(t *testing.T) {
	f := newRecoveryFixture(t, true)
	f.deliver(t, 1)
	old := f.state
	f.r.close()
	next := newCodexIngress(func(context.Context, broker.Message) error { t.Fatal("receipt replayed"); return nil })
	next.steer = f.r.steer
	next.recovery = &steerRecovery{store: f.store, scope: "scope", thread: recoveryThread, registration: "R", epoch: "E"}
	f.r = next
	f.r.initSteerRecovery()
	t.Cleanup(f.r.close)
	f.state = &messageAckState{store: f.store, scope: "scope", registration: "R", epoch: "E", lease: "L2", completed: map[uint64]bool{}, wake: make(chan struct{}, 1), failed: make(chan struct{})}
	f.binding.messageAck = f.state
	f.binding.identity.ChannelID = "L2"
	f.visible = true
	f.deliver(t, 1)
	f.deliver(t, 2)
	if old.through != 0 || f.state.through != 2 {
		t.Fatal("replacement retained old callback or lost record")
	}
}
func TestSteerRecoveryWatermarkWinsAfterClearFailure(t *testing.T) {
	f := newRecoveryFixture(t, true)
	if err := f.store.SaveDeliveryWatermark("scope", "R", "E", 2); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.store.Dir(), "steer-attempts", "v1")
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	f.r.recovery.loaded = false
	f.r.initSteerRecovery()
	if f.r.recoveryBlockedLocked() {
		t.Fatal("persisted watermark lost to failed intent clear")
	}
	f.deliver(t, 1)
	if len(f.pushed) != 1 {
		t.Fatal("completed stale intent blocks later sink")
	}
}

// A queued input can materialize at the head while a sliced history pass is open.
func TestSteerRecoveryQueueMaterializesAheadOfCursor(t *testing.T) {
	f := newRecoveryFixture(t, true)
	f.terminal = true
	phase, pages := 0, 0
	f.r.steer.dial = func(context.Context) (codexControl, error) {
		return scanControl{call: func(ctx context.Context, method string, args any) (json.RawMessage, error) {
			switch method {
			case "thread/items/list":
				p := args.(map[string]any)
				if phase == 1 && p["cursor"] == nil {
					return scanPage(scanMessages(), nil), nil
				}
				pages++
				if phase == 0 {
					return scanPage(nil, fmt.Sprintf("cursor-%d", pages)), nil
				}
				return scanPage(nil, nil), nil
			case "thread/queue/list":
				f.queueMatch = phase == 0
			}
			return f.call(ctx, method, args)
		}}, nil
	}
	f.deliver(t, 1)
	if len(f.pushed) != 0 {
		t.Fatal("partial scan released")
	}
	phase = 1
	f.deliver(t, 2)
	if len(f.pushed) != 0 {
		t.Fatal("queue-to-history head receipt missed; duplicate sink delivery")
	}
}

func TestSteerAttemptWriteOrderAndCrashCuts(t *testing.T) {
	for _, mode := range []string{"success", "retry", "save-fails", "reject", "clear-fails", "watermark-fails"} {
		t.Run(mode, func(t *testing.T) {
			if strings.HasSuffix(mode, "-fails") && os.Geteuid() == 0 {
				t.Skip("root bypasses read-only mode")
			}
			f := newRecoveryFixture(t, false)
			f.r.steer.gate = func() string { return "" }
			// Drive write ordering inline, without a competing timer callback.
			f.r.steer.timer = time.NewTimer(0)
			writeCodexHook(f.r, "PreToolUse", "T")
			native := &fakeSteerControl{}
			attempts := 0
			dir := filepath.Join(f.store.Dir(), "steer-attempts", "v1")
			delivery := filepath.Join(f.store.Dir(), "delivery")
			defer os.Chmod(dir, 0700)
			defer os.Chmod(delivery, 0700)
			f.r.steer.dial = func(context.Context) (codexControl, error) {
				return scanControl{call: func(ctx context.Context, m string, args any) (json.RawMessage, error) {
					if m != "turn/steer" {
						return native.Call(ctx, m, args)
					}
					attempts++
					record, err := f.store.LoadSteerAttempt("scope", recoveryThread)
					target := args.(map[string]any)["expectedTurnId"].(string)
					if err != nil || record.Pending == nil || !slices.Contains(record.Pending.Turns, target) {
						t.Fatal("steer bytes preceded durable turn intent")
					}
					if mode == "retry" && attempts == 1 {
						return nil, &steerRPCError{-32600, "expected active turn id T but found T2"}
					}
					if mode == "clear-fails" {
						if err := os.Chmod(dir, 0500); err != nil {
							t.Fatal(err)
						}
					}
					if mode == "watermark-fails" {
						if err := os.MkdirAll(delivery, 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.Chmod(delivery, 0500); err != nil {
							t.Fatal(err)
						}
					}
					if mode == "reject" || mode == "clear-fails" {
						return nil, &steerRPCError{-32600, "no active turn to steer"}
					}
					return json.Marshal(map[string]any{"turnId": target})
				}}, nil
			}
			if mode == "save-fails" {
				if err := os.Chmod(dir, 0500); err != nil {
					t.Fatal(err)
				}
			}
			m := scanMessages()[0]
			f.state.observe(1)
			meta := f.state.completion(t.Context(), f.binding, 1)
			err := f.r.deliver(context.WithValue(t.Context(), handoffKey{}, meta), m)
			if mode == "watermark-fails" {
				if err == nil || f.state.through != 0 {
					t.Fatal("failed watermark acknowledged")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if mode == "save-fails" && attempts != 0 {
				t.Fatal("save failure sent bytes")
			}
			os.Chmod(dir, 0700)
			os.Chmod(delivery, 0700)
			record, e := f.store.LoadSteerAttempt("scope", recoveryThread)
			if e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "success", "retry":
				if record.Pending != nil || f.state.through != 1 {
					t.Fatal("success did not watermark then clear")
				}
			case "reject":
				if record.Pending != nil {
					t.Fatal("rejection did not clear before fallback")
				}
			case "clear-fails", "watermark-fails":
				if record.Pending == nil || len(f.pushed) != 0 {
					t.Fatal("crash cut lost intent or opened sink")
				}
			}
			if mode == "retry" && attempts != 2 {
				t.Fatal("missing bounded retry")
			}
		})
	}
}

func TestSteerRecoveryDelayedOldRPCFencedBeforeReplacement(t *testing.T) {
	f := newRecoveryFixture(t, false)
	entered, release := make(chan struct{}), make(chan struct{})
	native := &fakeSteerControl{}
	f.r.steer.gate = func() string { return "" }
	f.r.steer.dial = func(context.Context) (codexControl, error) {
		return scanControl{call: func(ctx context.Context, m string, p any) (json.RawMessage, error) {
			if m != "turn/steer" {
				return native.Call(ctx, m, p)
			}
			close(entered)
			<-release
			return nil, context.DeadlineExceeded
		}}, nil
	}
	writeCodexHook(f.r, "PreToolUse", "T")
	sent := make(chan struct{})
	go func() { defer close(sent); f.deliver(t, 1) }()
	<-entered
	closed := make(chan struct{})
	go func() { f.r.close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("router released while old RPC could still write")
	default:
	}
	record, err := f.store.LoadSteerAttempt("scope", recoveryThread)
	if err != nil || record.Pending == nil {
		t.Fatal("delayed writer has no durable intent")
	}
	close(release)
	<-sent
	<-closed
	old := f.state
	f.r = newCodexIngress(func(context.Context, broker.Message) error { t.Fatal("delayed receipt reinjected"); return nil })
	f.r.steer = &codexSteerer{thread: recoveryThread, gate: func() string { return "off" }, dial: func(context.Context) (codexControl, error) {
		return scanControl{call: func(ctx context.Context, m string, p any) (json.RawMessage, error) {
			if m == "thread/items/list" {
				raw := scanPage(scanMessages()[:1], nil)
				return json.RawMessage(strings.ReplaceAll(string(raw), "xb:R:E:1-2", "xb:R:E:1-1")), nil
			}
			return f.call(ctx, m, p)
		}}, nil
	}}
	f.r.recovery = &steerRecovery{store: f.store, scope: "scope", thread: recoveryThread, registration: "R", epoch: "E"}
	f.r.initSteerRecovery()
	t.Cleanup(f.r.close)
	f.state = &messageAckState{store: f.store, scope: "scope", registration: "R", epoch: "E", lease: "L2", completed: map[uint64]bool{}, wake: make(chan struct{}, 1), failed: make(chan struct{})}
	f.binding.messageAck = f.state
	f.binding.identity.ChannelID = "L2"
	f.deliver(t, 1)
	if old.through != 0 || f.state.through != 1 {
		t.Fatal("old callback used or new receipt not acknowledged")
	}
}
