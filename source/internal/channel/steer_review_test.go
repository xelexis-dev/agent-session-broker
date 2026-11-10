package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/codexcontrol"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestDedupeReviewAIdlePositiveSerialReplay(t *testing.T) {
	f := newRecoveryFixture(t, true)
	f.visible = true
	f.terminal = true
	f.deliver(t, 1)
	record, err := f.store.LoadSteerAttempt("scope", recoveryThread)
	if err != nil || record.Pending == nil {
		t.Fatal("positive receipt cleared before final watermark")
	}
	f.deliver(t, 2)
	if len(f.pushed) != 0 || f.state.through != 2 {
		t.Fatal("serial positive replay duplicated or lost ACK", f.pushed, f.state.through)
	}
}
func TestDedupeReviewAPositiveCallbackFailureHolds(t *testing.T) {
	f := newRecoveryFixture(t, true)
	f.visible = true
	f.terminal = true
	f.state.observe(1)
	ctx := context.WithValue(t.Context(), handoffKey{}, handoffCompletion{registration: "R", epoch: "E", done: func() error { return context.Canceled }})
	if err := f.r.deliver(ctx, scanMessages()[0]); err != nil {
		t.Fatal(err)
	}
	f.deliver(t, 2)
	record, _ := f.store.LoadSteerAttempt("scope", recoveryThread)
	if record.Pending == nil || len(f.pushed) != 0 || f.state.through != 0 {
		t.Fatal("failed positive callback became negative release")
	}
}
func reviewLongControl() scanControl {
	return scanControl{call: func(_ context.Context, m string, args any) (json.RawMessage, error) {
		switch m {
		case "thread/turns/list":
			p := args.(map[string]any)
			start := 249
			if cursor, ok := p["cursor"].(string); ok {
				start, _ = strconv.Atoi(cursor)
			}
			limit := p["limit"].(int)
			data := []any{}
			n := start
			for ; n >= 0 && len(data) < limit; n-- {
				id := fmt.Sprintf("turn-%d", n)
				if n == 0 {
					id = "T"
				}
				data = append(data, map[string]any{"id": id, "status": "completed"})
			}
			var next any
			if n >= 0 {
				next = strconv.Itoa(n)
			}
			return json.Marshal(map[string]any{"data": data, "nextCursor": next})
		case "thread/items/list":
			return scanPage(nil, nil), nil
		case "thread/queue/list":
			return json.RawMessage(`{"data":[],"nextCursor":null}`), nil
		}
		return nil, errors.New("unexpected review fixture method")
	}}
}
func TestDedupeReviewB250TurnRelease(t *testing.T) {
	release, err := advanceSteerScan(t.Context(), reviewLongControl(), "U", scanRecord(), newSteerScan())
	if err != nil || !release {
		t.Fatal("250 completed turns permanently hold", err)
	}
}
func TestDedupeReviewB250TurnLiveConfirm(t *testing.T) {
	native := &fakeSteerControl{result: context.DeadlineExceeded}
	r, pushed, done := steerFixture(t, native)
	if err := steerMessage(r, done, 1); err != nil {
		t.Fatal(err)
	}
	r.steer.dial = func(context.Context) (codexControl, error) { return reviewLongControl(), nil }
	if err := r.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *pushed != 1 || *done != 1 {
		t.Fatal("live confirmation regressed on long thread")
	}
}
func reviewPagedControl(active, queued bool, last int) scanControl {
	return scanControl{call: func(_ context.Context, m string, args any) (json.RawMessage, error) {
		switch m {
		case "thread/turns/list":
			status := "completed"
			if active {
				status = "inProgress"
			}
			return json.Marshal(map[string]any{"data": []any{map[string]any{"id": "T", "status": status}}, "nextCursor": nil})
		case "thread/queue/list":
			if queued {
				return json.RawMessage(`{"data":[{}],"nextCursor":null}`), nil
			}
			return json.RawMessage(`{"data":[],"nextCursor":null}`), nil
		case "thread/items/list":
			p := args.(map[string]any)
			page := 1
			if c, ok := p["cursor"].(string); ok {
				page, _ = strconv.Atoi(c)
			}
			if page == last {
				return scanPage(scanMessages(), nil), nil
			}
			return scanPage(nil, strconv.Itoa(page+1)), nil
		}
		return nil, errors.New("unexpected review fixture method")
	}}
}
func TestDedupeReviewCPositiveProgressPastSlice(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprint(queued), func(t *testing.T) {
			scan := newSteerScan()
			c := reviewPagedControl(!queued, queued, 6)
			for i := 0; i < 3; i++ {
				advanceSteerScan(t.Context(), c, "U", scanRecord(), scan)
			}
			if len(scan.found) != 2 {
				t.Fatal("positive cursor resets behind active/queued frontier")
			}
		})
	}
}
func TestDedupeReviewD13000Items(t *testing.T) {
	scan := newSteerScan()
	base := reviewLongControl()
	c := scanControl{call: func(ctx context.Context, m string, p any) (json.RawMessage, error) {
		if m != "thread/items/list" {
			return base.Call(ctx, m, p)
		}
		args := p.(map[string]any)
		page := 1
		if cursor, ok := args["cursor"].(string); ok {
			page, _ = strconv.Atoi(cursor)
		}
		data := make([]any, 50)
		for i := range data {
			data[i] = map[string]any{"turnId": "T", "item": map[string]any{"type": "agentMessage"}}
		}
		var next any
		if page < 260 {
			next = strconv.Itoa(page + 1)
		}
		return json.Marshal(map[string]any{"data": data, "nextCursor": next})
	}}
	release := false
	for i := 0; i < 80 && !release; i++ {
		release, _ = advanceSteerScan(t.Context(), c, "U", scanRecord(), scan)
	}
	if !release {
		t.Fatal("13000 items hit a lifetime page ceiling")
	}
}
func TestDedupeReviewDAdaptivePage(t *testing.T) {
	s := newSteerScan()
	base := reviewPagedControl(true, false, 1)
	limits := []int{}
	c := scanControl{call: func(ctx context.Context, m string, p any) (json.RawMessage, error) {
		if m == "thread/items/list" {
			limit := p.(map[string]any)["limit"].(int)
			limits = append(limits, limit)
			if limit > 1 {
				return nil, codexcontrol.ErrTooLarge
			}
		}
		return base.Call(ctx, m, p)
	}}
	for i := 0; i < 4 && len(s.found) == 0; i++ {
		advanceSteerScan(t.Context(), c, "U", scanRecord(), s)
	}
	if len(s.found) != 2 {
		t.Fatal("oversize response never reduces page size", limits)
	}
}
func TestDedupeReviewDTimeoutRetainsCursor(t *testing.T) {
	s := newSteerScan()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	base := reviewPagedControl(true, false, 1)
	calls := 0
	c := scanControl{call: func(ctx context.Context, m string, p any) (json.RawMessage, error) {
		if m == "thread/items/list" {
			calls++
			if calls == 1 {
				return scanPage(nil, "tail"), nil
			}
			cancel()
			return nil, ctx.Err()
		}
		return base.Call(ctx, m, p)
	}}
	advanceSteerScan(ctx, c, "U", scanRecord(), s)
	if s.cursor != "tail" {
		t.Fatal("slice timeout erased cursor")
	}
}
func TestDedupeReviewEQueueUnsupportedBeforeIntent(t *testing.T) {
	f := newRecoveryFixture(t, false)
	native := &fakeSteerControl{}
	f.r.steer.gate = func() string { return "" }
	f.r.steer.dial = func(context.Context) (codexControl, error) {
		return scanControl{call: func(ctx context.Context, m string, p any) (json.RawMessage, error) {
			if m == "thread/queue/list" {
				return nil, &steerRPCError{-32601, "not supported"}
			}
			return native.Call(ctx, m, p)
		}}, nil
	}
	writeCodexHook(f.r, "PreToolUse", "T")
	f.deliver(t, 1)
	files, _ := filepath.Glob(filepath.Join(f.store.Dir(), "steer-attempts", "v1", "*.json"))
	if native.inputs != 0 || len(files) != 0 || !f.r.steer.disabled {
		t.Fatal("unsupported queue host received steer or intent", native.inputs, len(files))
	}
}
func TestDedupeReviewFStubDoesNotInstallRecovery(t *testing.T) {
	_, err := bindCodexControl(-1)(t.Context())
	if !errors.Is(err, codexcontrol.ErrNotDaemonHost) {
		t.Skip("non-posix stub or native overlay only")
	}
	runtime := newDetachedRuntime(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "steer-attempts"), []byte("unavailable"), 0600); err != nil {
		t.Fatal(err)
	}
	var router *codexIngress
	push := make(chan broker.Message, 1)
	h := runtime.harness(t, dir, 9031, func(o *durableOptions) {
		o.claudeInboxFactory = func(id broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
			router = newCodexIngress(func(_ context.Context, m broker.Message) error { push <- m; return nil })
			router.steer = &codexSteerer{dial: bindCodexControl(id.PID)}
			return router.deliver, router, nil
		}
	})
	h.attach(t)
	router.mu.Lock()
	installed := router.recovery != nil
	router.mu.Unlock()
	if installed {
		t.Fatal("non-posix control installed unavailable recovery")
	}
	if err := router.deliver(t.Context(), broker.Message{Body: "stub-fallback"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-push:
	default:
		t.Fatal("stub delivery blocked by store")
	}
}

func TestDedupeReviewAmbiguousPassCanRestart(t *testing.T) {
	scan := newSteerScan()
	base := reviewLongControl()
	bad := true
	c := scanControl{call: func(ctx context.Context, m string, p any) (json.RawMessage, error) {
		if m != "thread/items/list" {
			return base.Call(ctx, m, p)
		}
		if bad {
			raw := scanPage(scanMessages(), nil)
			var v map[string]any
			json.Unmarshal(raw, &v)
			item := v["data"].([]any)[0].(map[string]any)["item"].(map[string]any)
			item["content"] = []any{}
			return json.Marshal(v)
		}
		return scanPage(nil, nil), nil
	}}
	release, err := advanceSteerScan(t.Context(), c, "U", scanRecord(), scan)
	if err != nil || release || scan.negative == nil || !scan.negative.ambiguous {
		t.Fatal("ambiguous receipt was released")
	}
	bad = false
	release, err = advanceSteerScan(t.Context(), c, "U", scanRecord(), scan)
	if err != nil || !release || scan.negative.ambiguous {
		t.Fatal("fresh pass retained old ambiguity")
	}
}
func TestDedupeReviewHookScanBudget(t *testing.T) {
	f := newRecoveryFixture(t, true)
	f.r.steer.dial = func(ctx context.Context) (codexControl, error) { <-ctx.Done(); return nil, ctx.Err() }
	start := time.Now()
	f.r.steerHookDecision(t.Context())
	if time.Since(start) > 900*time.Millisecond || len(f.pushed) != 0 {
		t.Fatal("hook recovery scan exceeded short budget or delivered")
	}
}

func TestDedupeReviewDFrontierTimeoutRetainsNegativeCursor(t *testing.T) {
	s := newSteerScan()
	base := reviewPagedControl(false, false, 20)
	fail := false
	c := scanControl{call: func(ctx context.Context, m string, p any) (json.RawMessage, error) {
		if m == "thread/turns/list" && fail {
			return nil, context.DeadlineExceeded
		}
		return base.Call(ctx, m, p)
	}}
	advanceSteerScan(t.Context(), c, "U", scanRecord(), s)
	if s.negative == nil || s.negative.cursor == "" {
		t.Fatal("fixture did not open negative slice")
	}
	cursor := s.negative.cursor
	fail = true
	advanceSteerScan(t.Context(), c, "U", scanRecord(), s)
	if s.negative == nil || s.negative.cursor != cursor {
		t.Fatal("frontier timeout erased negative cursor")
	}
}

func TestDedupeReviewAPartialWatermarkRestartHolds(t *testing.T) {
	f := newRecoveryFixture(t, true)
	f.terminal = true
	if err := f.store.SaveDeliveryWatermark("scope", "R", "E", 1); err != nil {
		t.Fatal(err)
	}
	f.r.recovery.loaded = false
	f.r.initSteerRecovery()
	f.deliver(t, 2)
	record, _ := f.store.LoadSteerAttempt("scope", recoveryThread)
	if len(f.pushed) != 0 || record.Pending == nil || f.state.through != 1 {
		t.Fatal("partial completed receipt was released after restart")
	}
	f.visible = true
	f.r.flush(t.Context())
	if len(f.pushed) != 0 || f.state.through != 2 {
		t.Fatal("remaining replay did not complete from current proof")
	}
}
