package channel

import (
	"context"
	"encoding/json"
	"fmt"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSteerControl struct {
	mu               sync.Mutex
	inputTexts       []string
	inputs           int
	result           error
	results          []error
	generationPhase  bool
	paged            bool
	failPage         bool
	marker           string
	found            bool
	finished         bool
	release, entered chan struct{}
	pages            int
	calls            []string
}

func (f *fakeSteerControl) Close() error { return nil }
func (f *fakeSteerControl) Call(ctx context.Context, method string, p any) (json.RawMessage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	switch method {
	case "thread/turns/list":
		status := "inProgress"
		if f.finished {
			status = "completed"
		}
		f.mu.Unlock()
		return json.Marshal(map[string]any{"data": []any{map[string]any{"id": "T", "status": status}}, "nextCursor": nil})
	case "thread/items/list":
		f.pages++
		kind := "commandExecution"
		if f.generationPhase {
			kind = "agentMessage"
		}
		items := []any{map[string]any{"type": kind, "status": "inProgress"}}
		if f.found {
			items = append(items, map[string]any{"type": "userMessage", "clientId": f.marker, "content": []any{map[string]any{"type": "text", "text": f.inputTexts[len(f.inputTexts)-1]}}})
		}
		var next any
		if f.paged {
			args := p.(map[string]any)
			if (args["limit"] == 100 || args["limit"] == 50) && args["cursor"] == nil {
				items = []any{}
				next = "page2"
			} else if args["cursor"] != nil && f.failPage {
				f.mu.Unlock()
				return nil, context.DeadlineExceeded
			}
		}
		wrapped := make([]any, 0, len(items))
		for _, item := range items {
			turn := p.(map[string]any)["turnId"]
			if turn == nil {
				turn = "T"
			}
			wrapped = append(wrapped, map[string]any{"turnId": turn, "item": item})
		}
		f.mu.Unlock()
		return json.Marshal(map[string]any{"data": wrapped, "nextCursor": next})
	case "thread/queue/list":
		f.mu.Unlock()
		return json.RawMessage(`{"data":[],"nextCursor":null}`), nil
	case "turn/steer":
		f.inputs++
		args := p.(map[string]any)
		f.marker = args["clientUserMessageId"].(string)
		f.inputTexts = append(f.inputTexts, args["input"].([]any)[0].(map[string]any)["text"].(string))
		err := f.result
		if len(f.results) > 0 {
			err = f.results[0]
			f.results = f.results[1:]
		}
		release, entered := f.release, f.entered
		f.mu.Unlock()
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		if release != nil {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"turnId": args["expectedTurnId"]})
	}
	f.mu.Unlock()
	panic("unexpected method")
}
func steerFixture(t *testing.T, f *fakeSteerControl) (*codexIngress, *int, *int) {
	t.Helper()
	pushes, completed := new(int), new(int)
	r := newCodexIngress(func(context.Context, broker.Message) error { *pushes++; return nil })
	r.steer = &codexSteerer{dial: func(context.Context) (codexControl, error) { return f, nil }, gate: func() string { return "" }, thread: "U", delay: 10 * time.Second}
	writeCodexHook(r, "UserPromptSubmit", "T")
	t.Cleanup(r.close)
	return r, pushes, completed
}
func steerMessage(r *codexIngress, completed *int, seq uint64) error {
	ctx := context.WithValue(context.Background(), handoffKey{}, handoffCompletion{registration: "R", epoch: "E", done: func() error { *completed++; return nil }})
	return r.deliver(ctx, broker.Message{RegistrationID: "R", DeliverySequence: seq, Body: "peer", SentAt: time.Unix(1, int64(seq)).UTC(), FromRegistrationID: "F"})
}
func TestCodexSteerCore(t *testing.T) {
	f := &fakeSteerControl{}
	r, pushed, completed := steerFixture(t, f)
	if err := steerMessage(r, completed, 1); err != nil {
		t.Fatal(err)
	}
	if f.inputs != 1 || *completed != 1 || *pushed != 0 || len(r.pending) != 0 {
		t.Fatalf("steer=%d complete=%d queue=%d pending=%d", f.inputs, *completed, *pushed, len(r.pending))
	}
	if f.marker != "xb:R:E:1-1" {
		t.Fatal("wrong client id")
	}
}
func TestCodexSteerAmbiguousLeaseReplay(t *testing.T) {
	f := &fakeSteerControl{result: context.DeadlineExceeded}
	r, pushed, completed := steerFixture(t, f)
	steerMessage(r, completed, 1)
	r.nextGeneration()
	steerMessage(r, completed, 1)
	for _, event := range []string{"PostToolUse", "Stop"} {
		out, _, _, err := r.hookHandoff(event, "T")
		if err != nil || len(out) != 0 {
			t.Fatal("uncertain batch escaped to hook")
		}
	}
	if f.inputs != 1 || *completed != 0 || *pushed != 0 {
		t.Fatalf("uncertain replay: inputs=%d complete=%d copies=%d", f.inputs, *completed, *pushed)
	}
	f.mu.Lock()
	f.found = true
	f.mu.Unlock()
	r.steerDecision(t.Context(), false)
	if f.inputs != 1 || *completed != 1 || *pushed != 0 {
		t.Fatal("positive clientId proof did not complete exactly once")
	}
}
func TestCodexSteerDelayedAcceptanceAndFinishedAbsence(t *testing.T) {
	for _, finished := range []bool{false, true} {
		t.Run(map[bool]string{false: "late-accept", true: "finished-absent"}[finished], func(t *testing.T) {
			f := &fakeSteerControl{result: context.DeadlineExceeded}
			r, pushed, completed := steerFixture(t, f)
			steerMessage(r, completed, 1)
			writeCodexHook(r, "Stop", "T")
			r.steerDecision(t.Context(), false)
			if *completed != 0 || *pushed != 0 || f.inputs != 1 {
				t.Fatal("absence in active turn is not a rejection")
			}
			f.mu.Lock()
			f.finished = finished
			f.found = !finished
			f.mu.Unlock()
			r.steerDecision(t.Context(), false)
			want := 0
			if finished {
				want = 1
			}
			if *completed != 1 || *pushed != want || f.inputs != 1 {
				t.Fatal("confirmation lost/copied batch")
			}
		})
	}
}
func TestCodexSteerStopDuringRejectionAndNextBatch(t *testing.T) {
	f := &fakeSteerControl{result: &steerRPCError{-32600, "no active turn to steer"}, release: make(chan struct{}), entered: make(chan struct{}, 1)}
	r, pushed, completed := steerFixture(t, f)
	done := make(chan struct{})
	go func() { defer close(done); steerMessage(r, completed, 1) }()
	<-f.entered
	out, _, _, err := r.hookHandoff("Stop", "T")
	if err != nil || len(out) != 0 {
		t.Fatal("Stop took steer batch")
	}
	close(f.release)
	<-done
	if *pushed != 1 || *completed != 1 || f.inputs != 1 {
		t.Fatal("Stop rejection failed queue fallback")
	}
}
func TestCodexSteerGenerationFencesSuccess(t *testing.T) {
	f := &fakeSteerControl{release: make(chan struct{}), entered: make(chan struct{}, 1)}
	r, pushed, completed := steerFixture(t, f)
	done := make(chan struct{})
	go func() { defer close(done); steerMessage(r, completed, 1) }()
	<-f.entered
	r.nextGeneration()
	close(f.release)
	<-done
	if *completed != 0 || *pushed != 0 {
		t.Fatal("old generation completed")
	}
	steerMessage(r, completed, 1)
	if *completed != 1 || f.inputs != 1 || *pushed != 0 {
		t.Fatal("accepted old-generation replay copied")
	}
}
func TestCodexSteerGateAndUnsupported(t *testing.T) {
	for _, reason := range []string{"off", "unconfirmed-thread", "ack-required"} {
		t.Run(reason, func(t *testing.T) {
			f := &fakeSteerControl{}
			r, _, _ := steerFixture(t, f)
			r.steer.gate = func() string { return reason }
			steerMessage(r, new(int), 1)
			if len(f.calls) != 0 {
				t.Fatal("gate sent RPC")
			}
			if r.deliveryStatus().Reason != reason {
				t.Fatal("status gate")
			}
		})
	}
	f := &fakeSteerControl{result: &steerRPCError{-32601, "Method not found"}}
	r, _, _ := steerFixture(t, f)
	steerMessage(r, new(int), 1)
	if !r.steer.disabled {
		t.Fatal("unsupported method not disabled")
	}
}
func TestCodexSteerUnknownRejectionHolds(t *testing.T) {
	f := &fakeSteerControl{result: &steerRPCError{-32600, "a new rejection wording"}}
	r, pushed, completed := steerFixture(t, f)
	steerMessage(r, completed, 1)
	out, _ := writeCodexHook(r, "Stop", "T")
	if len(out) != 0 || *completed != 0 || *pushed != 0 || r.deliveryStatus().Reason != "steer-uncertain" {
		t.Fatal("unknown rejection treated as safe fallback")
	}
}
func TestCodexSteerResultClasses(t *testing.T) {
	for _, tc := range []struct {
		err        error
		kind, turn string
	}{
		{&steerRPCError{-32600, "no active turn to steer"}, "rejected", ""},
		{&steerRPCError{-32600, "thread not found: U"}, "rejected", ""},
		{&steerRPCError{-32600, "expected active turn id T1 but found T2"}, "retry", "T2"},
		{&steerRPCError{-32600, "unknown"}, "uncertain", ""},
		{&steerRPCError{-32601, "method missing"}, "unsupported", ""},
		{context.DeadlineExceeded, "uncertain", ""},
	} {
		kind, turn := steerRejection(tc.err)
		if kind != tc.kind || turn != tc.turn {
			t.Fatalf("got %s %s", kind, turn)
		}
	}
}
func TestCodexSteerGeneratingBatchTimerAndHook(t *testing.T) {
	for _, hookFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "hook-first", false: "timer-first"}[hookFirst], func(t *testing.T) {
			f := &fakeSteerControl{generationPhase: true}
			r, pushed, completed := steerFixture(t, f)
			r.steer.delay = 30 * time.Millisecond
			done := make(chan struct{})
			ctx := context.WithValue(t.Context(), handoffKey{}, handoffCompletion{registration: "R", epoch: "E", done: func() error { *completed++; close(done); return nil }})
			r.deliver(ctx, broker.Message{DeliverySequence: 1, Body: "timer"})
			f.mu.Lock()
			early := f.inputs
			f.mu.Unlock()
			if early != 0 {
				t.Fatal("generation steered before batch deadline")
			}
			if hookFirst {
				out, err := writeCodexHook(r, "PostToolUse", "T")
				if err != nil || len(out) == 0 {
					t.Fatal("hook failed to take timer batch")
				}
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("batch timer did not finish")
			}
			r.close()
			want := 1
			if hookFirst {
				want = 0
			}
			if f.inputs != want || *completed != 1 || *pushed != 0 {
				t.Fatal("timer/hook ownership")
			}
		})
	}
}
func TestCodexSteerRetriesOnlyExplicitTurnMismatch(t *testing.T) {
	f := &fakeSteerControl{results: []error{&steerRPCError{-32600, "expected active turn id T but found T2"}, nil}}
	r, pushed, completed := steerFixture(t, f)
	steerMessage(r, completed, 1)
	if f.inputs != 2 || *completed != 1 || *pushed != 0 {
		t.Fatal("explicit rejection not retried exactly once")
	}
}
func TestCodexSteerFinishedScanMustBeComplete(t *testing.T) {
	f := &fakeSteerControl{result: context.DeadlineExceeded, paged: true}
	r, pushed, completed := steerFixture(t, f)
	steerMessage(r, completed, 1)
	writeCodexHook(r, "Stop", "T")
	f.mu.Lock()
	f.finished = true
	f.failPage = true
	f.mu.Unlock()
	r.steerDecision(t.Context(), false)
	if *completed != 0 || *pushed != 0 {
		t.Fatal("partial scan released uncertain batch")
	}
	f.mu.Lock()
	f.failPage = false
	f.mu.Unlock()
	r.steerDecision(t.Context(), false)
	if *completed != 1 || *pushed != 1 {
		t.Fatal("full finished scan did not release")
	}
}
func TestCodexSteerConcurrentHooksAndLeaseReplay(t *testing.T) {
	for range 20 {
		f := &fakeSteerControl{result: context.DeadlineExceeded, release: make(chan struct{}), entered: make(chan struct{}, 1)}
		r, pushed, completed := steerFixture(t, f)
		done := make(chan struct{})
		go func() { defer close(done); steerMessage(r, completed, 1) }()
		<-f.entered
		var wg sync.WaitGroup
		for _, event := range []string{"PostToolUse", "Stop"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out, _, _, _ := r.hookHandoff(event, "T")
				if len(out) != 0 {
					t.Error("native input copied to hook")
				}
			}()
		}
		wg.Wait()
		r.nextGeneration()
		steerMessage(r, completed, 1)
		close(f.release)
		<-done
		if *completed != 0 || *pushed != 0 || f.inputs != 1 {
			t.Fatal("ambiguous native input copied/acknowledged")
		}
		f.mu.Lock()
		f.found = true
		f.mu.Unlock()
		r.steerDecision(t.Context(), false)
		if *completed != 1 || *pushed != 0 || f.inputs != 1 {
			t.Fatal("positive proof failed after lease replacement")
		}
		r.close()
	}
}
func TestCodexHookWriteOwnerReleasesToQueue(t *testing.T) {
	f := &fakeSteerControl{generationPhase: true}
	r, pushed, completed := steerFixture(t, f)
	steerMessage(r, completed, 1)
	out, done, _, err := r.hookHandoff("PostToolUse", "T")
	if err != nil || len(out) == 0 || done == nil {
		t.Fatal("hook handoff missing")
	}
	r.hookHandoff("Stop", "T")
	steerMessage(r, completed, 2)
	if *pushed != 0 {
		t.Fatal("queue crossed hook write")
	}
	if err := done(); err != nil {
		t.Fatal(err)
	}
	if *completed != 2 || *pushed != 1 {
		t.Fatal("hook release did not rerun decision")
	}
}

type schemaSteerReply struct{ raw string }

func (s schemaSteerReply) Call(context.Context, string, any) (json.RawMessage, error) {
	return json.RawMessage(s.raw), nil
}
func (s schemaSteerReply) Close() error { return nil }
func TestCodexSteerSchemaShapedEntry(t *testing.T) {
	// Literal ThreadItemsListResponse / ThreadItemEntry from installed 0.164 schema.
	c := schemaSteerReply{`{"data":[{"turnId":"T","startedAtMs":123,"completedAtMs":null,"item":{"type":"mcpToolCall","id":"call","server":"slow","tool":"hold","status":"inProgress","arguments":{},"result":null,"error":null}}],"nextCursor":null,"backwardsCursor":null}`}
	immediate, err := steerToolInProgress(t.Context(), c, "U", "T")
	if err != nil || !immediate {
		t.Fatal("schema-shaped tool entry was not recognized", err)
	}
	if _, err := steerToolInProgress(t.Context(), c, "U", "another-turn"); err == nil {
		t.Fatal("accepted another turn's item")
	}
}

func TestCodexSteerHookPhasePrimary(t *testing.T) {
	for _, event := range []string{"PreToolUse", "PostToolUse", "Stop", "Interrupt", "other-turn"} {
		t.Run(event, func(t *testing.T) {
			f := &fakeSteerControl{generationPhase: true}
			r, _, completed := steerFixture(t, f)
			writeCodexHook(r, "PreToolUse", "T")
			switch event {
			case "Interrupt":
				r.interrupt(t.Context())
				writeCodexHook(r, "UserPromptSubmit", "T")
			case "other-turn":
				writeCodexHook(r, "PreToolUse", "other")
			case "PostToolUse", "Stop":
				writeCodexHook(r, event, "T")
			}
			steerMessage(r, completed, 1)
			want := 0
			if event == "PreToolUse" {
				want = 1
			}
			if f.inputs != want {
				t.Fatal("wrong hook phase decision")
			}
		})
	}
}

func TestCodexSteerBurstWhileWritingPreservesFIFO(t *testing.T) {
	f := &fakeSteerControl{release: make(chan struct{}), entered: make(chan struct{}, 1)}
	r, pushed, completed := steerFixture(t, f)
	send := func(n int) error {
		ctx := context.WithValue(t.Context(), handoffKey{}, handoffCompletion{registration: "R", epoch: "E", done: func() error { *completed++; return nil }})
		return r.deliver(ctx, broker.Message{RegistrationID: "R", DeliverySequence: uint64(n), Body: fmt.Sprintf("BURST_%03d", n)})
	}
	done := make(chan error, 1)
	go func() { done <- send(1) }()
	<-f.entered
	for n := 2; n <= 17; n++ {
		if err := send(n); err != nil {
			t.Fatal(err)
		}
	}
	close(f.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if *completed != 17 || *pushed != 0 {
		t.Fatalf("completed=%d copies=%d", *completed, *pushed)
	}
	f.mu.Lock()
	text := strings.Join(f.inputTexts, "\n")
	f.mu.Unlock()
	previous := -1
	for n := 1; n <= 17; n++ {
		marker := fmt.Sprintf("BURST_%03d", n)
		at := strings.Index(text, marker)
		if strings.Count(text, marker) != 1 || at <= previous {
			t.Fatal("burst duplicated or reordered", n)
		}
		previous = at
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) != 0 {
		t.Fatal("burst stranded")
	}
}
