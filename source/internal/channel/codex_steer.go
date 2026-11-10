package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/codexcontrol"
	"regexp"
	"strings"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type codexControl interface {
	Call(context.Context, string, any) (json.RawMessage, error)
	Close() error
}
type steerRPCError struct {
	code    int
	message string
}

func (e *steerRPCError) Error() string { return "Codex control request rejected" }

// Codex 0.164 thread/items/list returns entries, not bare ThreadItem objects.
type codexTurnItem struct {
	TurnID string                                  `json:"turnId"`
	Item   struct{ Type, Status, ClientID string } `json:"item"`
}
type codexDelivery struct {
	Mode   string `json:"mode"`
	Reason string `json:"reason"`
}
type steerBatch struct {
	scan                                *steerScan
	rpcSent                             bool
	messages                            []broker.Message
	registration, epoch, turn, clientID string
	generation                          uint64
	delivered                           bool
	rejected                            bool
}
type codexSteerer struct {
	confirmed func() bool
	dial      func(context.Context) (codexControl, error)
	gate      func() string
	diagnose  func(string)
	thread    string
	reason    string
	disabled  bool
	fallback  bool
	delay     time.Duration
	timer     *time.Timer
	due       time.Time
	working   bool
	again     bool
	batch     *steerBatch
}

// A transport failure is uncertain only after turn/steer may have been sent.
func (r *codexIngress) steerDecision(ctx context.Context, fired bool) error {
	if held, e := r.recoveryDecision(ctx); held {
		return e
	}
	r.mu.Lock()
	s := r.steer
	if !r.closed && !r.active && r.owner == "" && len(r.pending) > 0 {
		callbacks, err := r.drainLocked(ctx)
		r.mu.Unlock()
		return finishQueueHandoffs(callbacks, err)
	}
	if s != nil && s.working {
		s.again = true
	}
	if s == nil || r.closed || s.working || r.owner == "hook-writing" || r.owner == "queue-writing" {
		r.mu.Unlock()
		return nil
	}
	s.working = true
	r.probes.Add(1)
	r.mu.Unlock()
	defer r.probes.Done()
	defer func() {
		r.mu.Lock()
		s.working = false
		again := s.again
		s.again = false
		r.mu.Unlock()
		if again {
			_ = r.steerDecision(ctx, false)
		}
	}()
	r.mu.Lock()
	batchPending := s.batch != nil
	r.mu.Unlock()
	if batchPending {
		return r.confirmSteer(ctx, s)
	}
	reason := s.gate()
	r.mu.Lock()
	if s.disabled {
		reason = "unsupported-codex"
	}
	if reason != "" {
		r.mu.Unlock()
		r.setSteerReason(s, reason)
		return nil
	}
	if len(r.pending) == 0 || !r.active || r.owner != "" || s.fallback {
		r.mu.Unlock()
		return nil
	}
	seq, generation := r.hookSeq, r.generation
	r.mu.Unlock()
	c, err := s.dial(ctx)
	if err != nil {
		r.setSteerReason(s, "unverified-host")
		return nil
	}
	defer c.Close()
	var queueErr error
	if probe, ok := c.(interface {
		CheckQueue(context.Context, string) error
	}); ok {
		queueErr = probe.CheckQueue(ctx, s.thread)
	} else {
		var raw json.RawMessage
		raw, queueErr = c.Call(ctx, "thread/queue/list", map[string]any{"threadId": s.thread, "limit": 1})
		if queueErr == nil {
			_, queueErr = codexcontrol.ParseQueuePage(raw)
		}
	}
	if queueErr != nil {
		var local *steerRPCError
		var rpc *codexcontrol.RPCError
		unsupported := errors.As(queueErr, &local) || errors.As(queueErr, &rpc) || errors.Is(queueErr, codexcontrol.ErrQueue)
		r.mu.Lock()
		if unsupported {
			s.disabled = true
			s.reason = "unsupported-codex"
		} else {
			s.reason = "unverified-host"
		}
		r.mu.Unlock()
		return nil
	}
	turn, active, err := steerActiveTurn(ctx, c, s.thread)
	if err != nil {
		r.steerReadFailure(s, err)
		return nil
	}
	r.mu.Lock()
	immediate := active && r.toolTurn != "" && r.toolTurn == turn
	r.mu.Unlock()
	if active && !immediate {
		immediate, err = steerToolInProgress(ctx, c, s.thread, turn)
		if err != nil {
			r.steerReadFailure(s, err)
			return nil
		}
	}
	r.mu.Lock()
	if r.closed || generation != r.generation || seq != r.hookSeq || r.owner != "" || len(r.pending) == 0 {
		r.mu.Unlock()
		return nil
	}
	s.reason = "steer-ready"
	if !active {
		r.active = false
		callbacks, e := r.drainLocked(ctx)
		r.mu.Unlock()
		return finishQueueHandoffs(callbacks, e)
	}
	if !immediate && !fired && time.Now().Before(s.due) {
		r.mu.Unlock()
		return nil
	}
	if !immediate && !fired && s.due.IsZero() {
		s.due = time.Now().Add(s.delay)
		r.armSteerTimerLocked(s)
		r.mu.Unlock()
		return nil
	}
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.due = time.Time{}
	batch := &steerBatch{messages: r.pending, turn: turn, generation: generation}
	first := r.metadata[r.pending[0].DeliverySequence]
	batch.registration, batch.epoch = first.registration, first.epoch
	batch.clientID = steerClientID(batch.registration, batch.epoch, batch.messages)
	r.pending = nil
	r.bytes = 0
	s.batch = batch
	r.owner = "steer-writing"
	r.mu.Unlock()
	content, contentErr := codexMessageContent(batch.messages)
	if contentErr != nil {
		return r.finishSteer(s, batch, false)
	}
	params := map[string]any{"threadId": s.thread, "expectedTurnId": turn, "input": []any{map[string]any{"type": "text", "text": content, "text_elements": []any{}}}, "clientUserMessageId": batch.clientID}
	if e := r.saveSteerAttempt(s, batch, batch.turn); e != nil {
		r.mu.Lock()
		if s.batch == batch {
			r.owner = "steer-uncertain"
			s.reason = "steer-recovery-pending"
		}
		r.mu.Unlock()
		return nil
	}
	r.mu.Lock()
	batch.rpcSent = true
	r.mu.Unlock()
	raw, err := c.Call(ctx, "turn/steer", params)
	kind, next := steerRejection(err)
	if err != nil && kind == "retry" {
		batch.turn = next
		params["expectedTurnId"] = next
		if e := r.saveSteerAttempt(s, batch, next); e != nil {
			r.mu.Lock()
			if s.batch == batch {
				r.owner = "steer-uncertain"
				s.reason = "steer-recovery-pending"
			}
			r.mu.Unlock()
			return nil
		}
		raw, err = c.Call(ctx, "turn/steer", params)
		kind, _ = steerRejection(err)
		if kind == "retry" {
			kind = "rejected"
		}
	}
	if err == nil {
		var result struct {
			TurnID string `json:"turnId"`
		}
		if json.Unmarshal(raw, &result) == nil && result.TurnID == batch.turn {
			return r.finishSteer(s, batch, true)
		}
		kind = "uncertain"
	}
	if kind == "rejected" || kind == "unsupported" {
		r.mu.Lock()
		if kind == "unsupported" && s.batch == batch {
			s.disabled = true
			s.reason = "unsupported-codex"
		}
		r.mu.Unlock()
		return r.finishSteer(s, batch, false)
	}
	r.mu.Lock()
	if s.batch != batch || r.closed {
		r.mu.Unlock()
		return nil
	}
	r.owner = "steer-uncertain"
	s.reason = "steer-uncertain"
	r.mu.Unlock()
	return r.confirmSteer(ctx, s)
}
func (r *codexIngress) armSteerTimerLocked(s *codexSteerer) {
	if s.timer != nil {
		return
	}
	s.timer = time.AfterFunc(time.Until(s.due), func() {
		r.mu.Lock()
		s.timer = nil
		r.mu.Unlock()
		_ = r.steerDecision(context.Background(), true)
	})
}
func (r *codexIngress) setSteerReason(s *codexSteerer, reason string) {
	r.mu.Lock()
	changed := s.reason != reason
	s.reason = reason
	diagnose := s.diagnose
	r.mu.Unlock()
	if changed && diagnose != nil {
		code := "steer-disabled-" + reason
		if reason == "unverified-host" {
			code = "steer-unverified"
		}
		diagnose(code)
	}
}
func (r *codexIngress) steerReadFailure(s *codexSteerer, err error) {
	kind, _ := steerRejection(err)
	if kind == "unsupported" {
		r.mu.Lock()
		s.disabled = true
		r.mu.Unlock()
		r.setSteerReason(s, "unsupported-codex")
	} else {
		r.setSteerReason(s, "unverified-host")
	}
}
func steerActiveTurn(ctx context.Context, c codexControl, thread string) (string, bool, error) {
	raw, err := c.Call(ctx, "thread/turns/list", map[string]any{"threadId": thread, "limit": 1})
	if err != nil {
		return "", false, err
	}
	var out struct {
		Data []struct{ ID, Status string } `json:"data"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Data == nil {
		return "", false, errors.New("invalid turns response")
	}
	if len(out.Data) == 0 {
		return "", false, nil
	}
	if out.Data[0].ID == "" {
		return "", false, errors.New("invalid turn id")
	}
	switch out.Data[0].Status {
	case "inProgress":
		return out.Data[0].ID, true, nil
	case "completed", "failed", "interrupted":
		return out.Data[0].ID, false, nil
	default:
		return "", false, errors.New("unknown turn state")
	}
}
func steerToolInProgress(ctx context.Context, c codexControl, thread, turn string) (bool, error) {
	raw, err := c.Call(ctx, "thread/items/list", map[string]any{"threadId": thread, "turnId": turn, "sortDirection": "desc", "limit": 1})
	if err != nil {
		return false, err
	}
	var out struct {
		Data []codexTurnItem `json:"data"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Data == nil {
		return false, errors.New("invalid items response")
	}
	if len(out.Data) == 0 {
		return false, nil
	}
	if out.Data[0].TurnID != turn {
		return false, errors.New("unexpected item turn")
	}
	item := out.Data[0].Item
	switch item.Type {
	case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "collabAgentToolCall", "imageGeneration":
		return item.Status == "inProgress", nil
	}
	return false, nil
}
func (r *codexIngress) confirmSteer(ctx context.Context, s *codexSteerer) error {
	r.mu.Lock()
	batch := s.batch
	a := r.recovery
	if batch == nil {
		r.mu.Unlock()
		return nil
	}
	if a != nil && a.scan != nil {
		batch.scan = a.scan
	}
	if batch.scan == nil {
		batch.scan = newSteerScan()
	}
	scan := batch.scan
	record := clientstate.SteerAttemptRecord{Registration: batch.registration, Epoch: batch.epoch, Pending: &clientstate.SteerAttempt{ClientID: batch.clientID, Turns: []string{batch.turn}}}
	for _, m := range batch.messages {
		record.Pending.Sequences = append(record.Pending.Sequences, m.DeliverySequence)
	}
	if a != nil && a.record.Pending != nil {
		record = a.record
	}
	delivered, rejected, noSend := batch.delivered, batch.rejected, a != nil && !batch.rpcSent
	r.mu.Unlock()
	if delivered {
		return r.finishSteer(s, batch, true)
	}
	if rejected || noSend {
		return r.finishSteer(s, batch, false)
	}
	ctx, cancel := steerScanContext(ctx)
	defer cancel()
	c, err := s.dial(ctx)
	if err != nil {
		return nil
	}
	defer c.Close()
	release, err := advanceSteerScan(ctx, c, s.thread, record, scan)
	if scan.ambiguous || scan.negative != nil && scan.negative.ambiguous {
		r.recoveryDiagnostic("steer-recovery-ambiguous")
	}
	found := true
	for _, m := range batch.messages {
		proof, ok := scan.found[m.DeliverySequence]
		if !ok || !sameSteerEnvelope(m, proof) {
			found = false
			break
		}
	}
	if found {
		err := r.finishSteer(s, batch, true)
		r.mu.Lock()
		acked := a != nil && r.recovery == a && a.resolved
		r.mu.Unlock()
		if acked {
			r.recoveryDiagnostic("steer-recovery-acked")
		}
		return err
	}
	if err != nil {
		return nil
	}
	if release && batch.rpcSent {
		_, active, e := steerActiveTurn(ctx, c, s.thread)
		if e != nil {
			return nil
		}
		r.mu.Lock()
		if s.batch == batch && !r.closed {
			r.active = active
		}
		r.mu.Unlock()
		return r.finishSteer(s, batch, false)
	}

	return nil
}
func (r *codexIngress) finishSteer(s *codexSteerer, batch *steerBatch, delivered bool) error {
	r.mu.Lock()
	a := r.recovery
	current := s.batch == batch
	if delivered && current && a != nil && a.scan != nil && a.record.Pending != nil && a.record.Pending.ClientID == batch.clientID {
		a.scan.positive = true
	}
	r.mu.Unlock()
	if !delivered && a != nil && current && !r.clearSteerAttempt(a, batch.clientID) {
		r.mu.Lock()
		if s.batch == batch {
			batch.rejected = true
			r.owner = "steer-uncertain"
			s.reason = "steer-recovery-pending"
		}
		r.mu.Unlock()
		return nil
	}

	r.mu.Lock()
	if s.batch != batch || batch == nil {
		r.mu.Unlock()
		return nil
	}
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	var done func() error
	if batch.generation != r.generation {
		batch.delivered = delivered
		batch.rejected = !delivered
		s.reason = "steer-uncertain"
		r.owner = "steer-uncertain"
		r.mu.Unlock()
		return nil
	}
	if delivered {
		batch.delivered = true
		done = r.takeCompletionsLocked(batch.messages)
	} else {
		// Old lease callbacks are gone; replay will re-establish completion ownership.
		s.fallback = true
		r.pending = append(batch.messages, r.pending...)
		r.recountLocked()
	}
	s.batch = nil
	r.owner = ""
	s.again = len(r.pending) > 0
	if !s.disabled {
		s.reason = "steer-ready"
	}
	diagnose := s.diagnose
	callbacks := []func() error{}
	if done != nil {
		callbacks = append(callbacks, done)
	}
	var err error
	if !r.active {
		var queued []func() error
		queued, err = r.drainLocked(context.Background())
		callbacks = append(callbacks, queued...)
	}
	r.mu.Unlock()
	if diagnose != nil {
		if delivered {
			diagnose("steer-delivered")
		} else {
			diagnose("steer-rejected")
		}
	}
	err = finishQueueHandoffs(callbacks, err)
	if delivered && a != nil && err == nil {
		r.clearCompletedSteer(a)
	}
	return err
}
func (r *codexIngress) recountLocked() {
	r.bytes = 0
	for _, m := range r.pending {
		raw, _ := json.Marshal(m)
		r.bytes += len(raw)
	}
}

// Replay is intercepted before every sink, including queue/follow paths.
func (r *codexIngress) interceptSteerReplay(ctx context.Context, m broker.Message) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.steer == nil || r.steer.batch == nil {
		return false
	}
	batch := r.steer.batch
	meta := handoff(ctx)
	if meta.registration != batch.registration || meta.epoch != batch.epoch {
		return false
	}
	for _, old := range batch.messages {
		if old.DeliverySequence == m.DeliverySequence {
			if meta.done != nil {
				r.completions[m.DeliverySequence] = meta.done
				meta.generation = r.generation
				r.metadata[m.DeliverySequence] = meta
			}
			all := true
			for _, held := range batch.messages {
				if r.metadata[held.DeliverySequence].generation != r.generation {
					all = false
				}
			}
			if all {
				batch.generation = r.generation
			}
			return true
		}
	}
	return false
}
func (r *codexIngress) deliveryStatus() *codexDelivery {
	r.mu.Lock()
	s := r.steer
	if s == nil {
		r.mu.Unlock()
		return nil
	}
	if r.recoveryBlockedLocked() {
		reason := "steer-recovery-pending"
		if r.recovery.unavailable {
			reason = "steer-recovery-unavailable"
		}
		r.mu.Unlock()
		return &codexDelivery{Mode: "steer", Reason: reason}
	}
	reason, disabled := s.reason, s.disabled
	uncertain := s.batch != nil && r.owner == "steer-uncertain"
	r.mu.Unlock()
	if uncertain {
		return &codexDelivery{Mode: "steer", Reason: "steer-uncertain"}
	}
	if gate := s.gate(); gate != "" {
		return &codexDelivery{Mode: "hook", Reason: gate}
	}
	if disabled {
		return &codexDelivery{Mode: "hook", Reason: "unsupported-codex"}
	}
	if reason == "steer-ready" {
		return &codexDelivery{Mode: "steer", Reason: reason}
	}
	if reason != "unverified-host" {
		reason = "unverified-host"
		if codexControlAvailable {
			reason = "steer-unchecked"
		}
	}
	return &codexDelivery{Mode: "hook", Reason: reason}
}

func codexMessageContent(messages []broker.Message) (string, error) {
	projected := make([]broker.Message, len(messages))
	for i, m := range messages {
		projected[i] = broker.LocalMessage(m)
	}
	data, err := json.Marshal(projected)
	return codexEnvelopePrefix + string(data), err
}

var expectedSteerTurn = regexp.MustCompile(`^expected active turn id .+[,;]? (?:but )?found (\S+)$`)

func steerRejection(err error) (kind, turn string) {
	var rpc *steerRPCError
	if !errors.As(err, &rpc) {
		return "uncertain", ""
	}
	if rpc.code == -32601 {
		return "unsupported", ""
	}
	if rpc.code != -32600 {
		return "uncertain", ""
	}
	if rpc.message == "no active turn to steer" || strings.HasPrefix(rpc.message, "thread not found:") {
		return "rejected", ""
	}
	if m := expectedSteerTurn.FindStringSubmatch(rpc.message); m != nil {
		return "retry", strings.Trim(m[1], `"'`)
	}
	return "uncertain", ""
}
func steerClientID(registration, epoch string, m []broker.Message) string {
	return fmt.Sprintf("xb:%s:%s:%d-%d", registration, epoch, m[0].DeliverySequence, m[len(m)-1].DeliverySequence)
}

func (r *codexIngress) resetSteerTimerLocked() {
	if s := r.steer; s != nil {
		if s.timer != nil {
			s.timer.Stop()
			s.timer = nil
		}
		s.due = time.Time{}
		s.fallback = false
	}
}

func (r *codexIngress) steerHookDecision(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	return r.steerDecision(ctx, false)
}
