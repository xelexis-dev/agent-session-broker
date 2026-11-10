package channel

import (
	"context"
	"encoding/json"
	"errors"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"sync"
	"time"
)

// A synchronous MCP hook owns a model boundary until its result is returned.
// Serializing Stop with delivery makes every message belong to either that
// boundary or the native input queue, never both.
type codexIngress struct {
	recovery    *steerRecovery
	steer       *codexSteerer
	owner       string
	metadata    map[uint64]handoffCompletion
	completions map[uint64]func() error
	mu          sync.Mutex
	activeTurn  string
	toolTurn    string
	hookSeq     uint64
	generation  uint64
	closed      bool
	probe       func(context.Context, string) bool
	onRecovery  func()
	probes      sync.WaitGroup
	active      bool
	pending     []broker.Message
	bytes       int
	push        func(context.Context, broker.Message) error
}

func newCodexIngress(push func(context.Context, broker.Message) error) *codexIngress {
	return &codexIngress{push: push, completions: map[uint64]func() error{}, metadata: map[uint64]handoffCompletion{}}
}
func (r *codexIngress) deliver(ctx context.Context, m broker.Message) error {
	m = broker.LocalMessage(m)
	if r.interceptSteerReplay(ctx, m) {
		return r.steerDecision(ctx, false)
	}
	data, err := json.Marshal(m)
	if err != nil || m.Batch != nil || len(data) > 12000 {
		return errors.New("Codex ingress envelope exceeds 12000 bytes or contains a batch")
	}
	if held, e := r.recoveryIntake(ctx, m); held {
		return e
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("Codex ingress is closed")
	}
	if !r.active && r.owner == "" {
		err := r.push(ctx, m)
		r.mu.Unlock()
		if err != nil {
			return err
		}
		if done := handoff(ctx).done; done != nil {
			return done()
		}
		return nil
	}
	if len(r.pending) >= 32 || r.bytes+len(data) > 64*1024 {
		r.mu.Unlock()
		return errors.New("Codex ingress pending queue is full")
	}
	if done := handoff(ctx).done; done != nil {
		if r.completions == nil {
			r.completions = map[uint64]func() error{}
		}
		r.completions[m.DeliverySequence] = done
		meta := handoff(ctx)
		meta.generation = r.generation
		r.metadata[m.DeliverySequence] = meta
	}
	r.pending = append(r.pending, m)
	if r.steer != nil && r.steer.due.IsZero() {
		r.steer.due = time.Now().Add(r.steer.delay)
		r.armSteerTimerLocked(r.steer)
	}
	r.bytes += len(data)
	r.mu.Unlock()
	return r.flush(ctx)
}
func (r *codexIngress) takeCompletionsLocked(messages []broker.Message) func() error {
	var callbacks []func() error
	for _, m := range messages {
		if done := r.completions[m.DeliverySequence]; done != nil {
			callbacks = append(callbacks, done)
			delete(r.completions, m.DeliverySequence)
			delete(r.metadata, m.DeliverySequence)
		}
	}
	if len(callbacks) == 0 {
		return nil
	}
	generation := r.generation
	return func() error {
		r.mu.Lock()
		valid := !r.closed && r.generation == generation
		r.mu.Unlock()
		if !valid {
			return context.Canceled
		}
		for _, done := range callbacks {
			if err := done(); err != nil {
				return err
			}
		}
		return nil
	}
}
func (r *codexIngress) hookHandoff(event string, turnIDs ...string) (map[string]any, func() error, func(), error) {
	switch event {
	case "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop":
	default:
		return nil, nil, nil, errors.New("unsupported ingress hook event")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, nil, nil, errors.New("Codex ingress is closed")
	}
	r.hookSeq++
	r.activeTurn = ""
	if len(turnIDs) > 0 {
		r.activeTurn = turnIDs[0]
	}
	switch event {
	case "PreToolUse":
		r.toolTurn = r.activeTurn
	case "PostToolUse", "Stop":
		r.toolTurn = ""
	case "UserPromptSubmit":
		r.toolTurn = ""
	}
	r.active = event != "Stop" || (r.owner == "" && len(r.pending) > 0)
	out := map[string]any{}
	if r.recoveryBlockedLocked() && r.owner == "" {
		r.owner = "steer-recovery"
	}
	if r.owner != "" || len(r.pending) == 0 {
		return out, nil, nil, nil
	}
	content, err := codexMessageContent(r.pending)
	if err != nil {
		return nil, nil, nil, err
	}
	if event == "Stop" {
		out["decision"] = "block"
		out["reason"] = content
	} else {
		out["hookSpecificOutput"] = map[string]any{"hookEventName": event, "additionalContext": content}
	}
	messages := r.pending
	r.owner = "hook-writing"
	generation := r.generation
	var once sync.Once
	finish := func(written bool) (err error) {
		once.Do(func() {
			r.mu.Lock()
			var complete func() error
			if !r.closed && r.generation == generation && r.owner == "hook-writing" {
				r.owner = ""
				if written {
					complete = r.takeCompletionsLocked(messages)
				} else {
					r.pending = append(messages, r.pending...)
					r.recountLocked()
				}
			}
			r.mu.Unlock()
			if complete != nil {
				err = complete()
			}
			_ = r.steerHookDecision(context.Background())
		})
		return err
	}
	r.pending = nil
	r.resetSteerTimerLocked()
	r.bytes = 0
	return out, func() error { return finish(true) }, func() { _ = finish(false) }, nil
}

// The binding fence can cover mutation without running completion callbacks under it.
func (r *codexIngress) interruptHandoff(ctx context.Context) ([]func() error, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("Codex ingress is closed")
	}
	r.hookSeq++
	r.activeTurn = ""
	r.toolTurn = ""
	r.active = false
	callbacks, err := r.drainLocked(ctx)
	r.mu.Unlock()
	return callbacks, err
}
func finishQueueHandoffs(callbacks []func() error, err error) error {
	for _, done := range callbacks {
		if completionErr := done(); completionErr != nil && err == nil {
			err = completionErr
		}
	}
	return err
}
func (r *codexIngress) drainLocked(ctx context.Context) ([]func() error, error) {
	if r.recoveryBlockedLocked() {
		return nil, nil
	}
	if r.owner != "" {
		return nil, nil
	}
	r.owner = "queue-writing"
	defer func() { r.owner = "" }()
	var callbacks []func() error
	for len(r.pending) > 0 {
		data, _ := json.Marshal(r.pending[0])
		if err := r.push(ctx, r.pending[0]); err != nil {
			return callbacks, err
		}
		if done := r.takeCompletionsLocked(r.pending[:1]); done != nil {
			callbacks = append(callbacks, done)
		}
		r.bytes -= len(data)
		r.pending = r.pending[1:]
	}
	r.bytes = 0
	r.resetSteerTimerLocked()
	return callbacks, nil
}

func (r *codexIngress) flush(ctx context.Context) error {
	if err := r.steerDecision(ctx, false); err != nil {
		return err
	}
	r.mu.Lock()
	if r.closed || r.owner != "" || !r.active || len(r.pending) == 0 || r.activeTurn == "" || r.probe == nil {
		r.mu.Unlock()
		return nil
	}
	seq, turn, generation, probe := r.hookSeq, r.activeTurn, r.generation, r.probe
	r.probes.Add(1)
	r.mu.Unlock()
	defer r.probes.Done()
	probeCtx, cancel := context.WithTimeout(ctx, activityBudget)
	ended := probe(probeCtx, turn)
	valid := probeCtx.Err() == nil
	cancel()
	if !ended || !valid {
		return nil
	}
	r.mu.Lock()
	if r.closed || r.owner != "" || !r.active || r.hookSeq != seq || r.activeTurn != turn || r.generation != generation || ctx.Err() != nil {
		r.mu.Unlock()
		return nil
	}
	r.active = false
	callbacks, err := r.drainLocked(ctx)
	diagnose := r.onRecovery
	r.mu.Unlock()
	if diagnose != nil {
		diagnose()
	}
	return finishQueueHandoffs(callbacks, err)
}

// Reauthorization may restart the same binding only after its old receiver joined.
func (r *codexIngress) startReceiver() {
	r.mu.Lock()
	r.closed = false
	r.generation++
	r.mu.Unlock()
}

func (r *codexIngress) nextGeneration() {
	r.mu.Lock()
	if r.recovery != nil && r.recovery.record.Pending != nil && !r.recovery.resolved && r.steer != nil {
		r.steer.batch = nil
		r.owner = "steer-recovery"
	}
	kept := r.pending[:0]
	for _, m := range r.pending {
		if m.DeliverySequence == 0 {
			kept = append(kept, m)
		} else {
			delete(r.completions, m.DeliverySequence)
			delete(r.metadata, m.DeliverySequence)
		}
	}
	r.pending = kept
	r.bytes = 0
	for _, m := range kept {
		data, _ := json.Marshal(m)
		r.bytes += len(data)
	}
	r.generation++
	if r.owner == "hook-writing" {
		r.owner = ""
	}
	r.mu.Unlock()
}

func (r *codexIngress) close() {
	r.mu.Lock()
	r.closed = true
	r.resetSteerTimerLocked()
	r.generation++
	r.mu.Unlock()
	r.probes.Wait()
}

// A removed registration cannot complete or replay an envelope into its replacement.
func (r *codexIngress) bindRegistration(registration, epoch string) bool {
	r.mu.Lock()
	if a := r.recovery; a != nil && (a.registration != registration || a.epoch != epoch) {
		r.recovery = &steerRecovery{store: a.store, scope: a.scope, thread: a.thread, registration: registration, epoch: epoch}
	}
	s := r.steer
	if s == nil || s.batch == nil || (s.batch.registration == registration && s.batch.epoch == epoch) {
		r.mu.Unlock()
		return false
	}
	for _, m := range s.batch.messages {
		delete(r.completions, m.DeliverySequence)
		delete(r.metadata, m.DeliverySequence)
	}
	s.batch = nil
	s.reason = "steer-ready"
	s.fallback = false
	r.owner = ""
	r.mu.Unlock()
	return true
}
