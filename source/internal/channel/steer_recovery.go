package channel

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/codexcontrol"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type steerRecovery struct {
	store                              *clientstate.Store
	scope, thread, registration, epoch string
	loaded, unavailable, resolved      bool
	record                             clientstate.SteerAttemptRecord
	scan                               *steerScan
}

func (r *codexIngress) recoveryBlockedLocked() bool {
	a := r.recovery
	return a != nil && (!a.loaded || a.unavailable || (!a.resolved && a.record.Pending != nil && (r.steer == nil || r.steer.batch == nil)))
}
func (r *codexIngress) recoveryDiagnostic(code string) {
	r.mu.Lock()
	var f func(string)
	if r.steer != nil {
		f = r.steer.diagnose
	}
	r.mu.Unlock()
	if f != nil {
		f(code)
	}
}

// The receiver calls this before opening its stream, outside the binding lock.
func (r *codexIngress) initSteerRecovery() {
	r.mu.Lock()
	a := r.recovery
	if a == nil || a.loaded && !a.unavailable {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	record, err := a.store.LoadSteerAttempt(a.scope, a.thread)
	resolved := record.Pending == nil
	positive := false
	if err == nil && record.Registration != "" && (record.Registration != a.registration || record.Epoch != a.epoch) {
		next := record
		next.Registration = a.registration
		next.Epoch = a.epoch
		next.Pending = nil
		err = a.store.SaveSteerAttempt(a.scope, a.thread, record, next)
		if err == nil {
			record = next
			resolved = true
		}
	}
	if err == nil && record.Pending != nil {
		through, e := a.store.LoadDeliveryWatermark(a.scope, a.registration, a.epoch)
		err = e
		if e == nil && through >= record.Pending.Sequences[0] {
			positive = true
		}
		if e == nil && through >= record.Pending.Sequences[len(record.Pending.Sequences)-1] {
			next := record
			next.Pending = nil
			e = a.store.SaveSteerAttempt(a.scope, a.thread, record, next)
			if e == nil {
				record = next
			}
			if errors.Is(e, clientstate.ErrSteerChanged) {
				err = e
			} else {
				resolved = true
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recovery != a {
		return
	}
	a.loaded = true
	a.unavailable = err != nil
	a.record = record
	a.resolved = resolved && err == nil
	a.scan = newSteerScan()
	a.scan.positive = positive
	if a.unavailable || !a.resolved {
		if r.owner == "" {
			r.owner = "steer-recovery"
		}
	} else if r.owner == "steer-recovery" {
		r.owner = ""
	}
}
func (r *codexIngress) saveSteerAttempt(s *codexSteerer, b *steerBatch, turn string) error {
	r.mu.Lock()
	a := r.recovery
	if a == nil {
		r.mu.Unlock()
		return nil
	}
	if r.closed || s.batch != b || r.generation != b.generation || !a.loaded || a.unavailable {
		r.mu.Unlock()
		return clientstate.ErrSteerRecovery
	}
	before := a.record
	next := before
	next.Registration = b.registration
	next.Epoch = b.epoch
	seq := make([]uint64, 0, len(b.messages))
	for _, m := range b.messages {
		seq = append(seq, m.DeliverySequence)
	}
	turns := []string{turn}
	if before.Pending != nil {
		if before.Pending.ClientID != b.clientID && !a.resolved {
			r.mu.Unlock()
			return clientstate.ErrSteerRecovery
		}
		if before.Pending.ClientID == b.clientID {
			turns = append([]string{}, before.Pending.Turns...)
			if !slices.Contains(turns, turn) {
				turns = append(turns, turn)
			}
		}
	}
	next.Pending = &clientstate.SteerAttempt{ClientID: b.clientID, Sequences: seq, Turns: turns}
	r.mu.Unlock()
	err := a.store.SaveSteerAttempt(a.scope, a.thread, before, next)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recovery != a || s.batch != b || r.closed {
		return context.Canceled
	}
	if err != nil {
		a.unavailable = true
		s.reason = "steer-recovery-unavailable"
		return err
	}
	a.record = next
	a.resolved = false
	a.scan = newSteerScan()
	return nil
}
func (r *codexIngress) clearSteerAttempt(a *steerRecovery, expected string) bool {
	r.mu.Lock()
	if r.recovery != a {
		r.mu.Unlock()
		return false
	}
	before := a.record
	positive := a.scan != nil && (a.scan.positive || len(a.scan.found) > 0)
	if before.Pending != nil && expected != "" && before.Pending.ClientID != expected {
		r.mu.Unlock()
		return false
	}
	next := before
	next.Pending = nil
	r.mu.Unlock()
	if before.Pending != nil {
		through, e := a.store.LoadDeliveryWatermark(a.scope, a.registration, a.epoch)
		if e != nil || (positive || through >= before.Pending.Sequences[0]) && through < before.Pending.Sequences[len(before.Pending.Sequences)-1] {
			return false
		}
	}
	err := a.store.SaveSteerAttempt(a.scope, a.thread, before, next)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recovery != a {
		return false
	}
	if err != nil {
		if errors.Is(err, clientstate.ErrSteerChanged) {
			a.loaded = false
		}
		return false
	}
	a.record = next
	a.resolved = true
	a.unavailable = false
	if r.owner == "steer-recovery" {
		r.owner = ""
	}
	return true
}

// A watermark already persisted by the normal callback wins over a stale intent.
func (r *codexIngress) clearCompletedSteer(a *steerRecovery) bool {
	r.mu.Lock()
	if r.recovery != a {
		r.mu.Unlock()
		return false
	}
	if a.record.Pending == nil {
		r.mu.Unlock()
		return true
	}
	last := a.record.Pending.Sequences[len(a.record.Pending.Sequences)-1]
	r.mu.Unlock()
	through, err := a.store.LoadDeliveryWatermark(a.scope, a.registration, a.epoch)
	if err != nil || through < last {
		return false
	}
	if r.clearSteerAttempt(a, "") {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recovery != a || !a.loaded {
		return false
	}
	a.resolved = true
	a.unavailable = false
	if r.owner == "steer-recovery" {
		r.owner = ""
	}
	return true
}
func (r *codexIngress) recoveryIntake(ctx context.Context, m broker.Message) (bool, error) {
	r.mu.Lock()
	if !r.recoveryBlockedLocked() {
		r.mu.Unlock()
		return false, nil
	}
	if r.closed {
		r.mu.Unlock()
		return true, context.Canceled
	}

	found := false
	for _, old := range r.pending {
		if old.DeliverySequence == m.DeliverySequence && old.RegistrationID == m.RegistrationID {
			found = true
			break
		}
	}
	if !found {
		data, _ := json.Marshal(m)
		if len(r.pending) >= 32 || r.bytes+len(data) > 64*1024 {
			r.mu.Unlock()
			return true, errors.New("Codex ingress pending queue is full")
		}
		r.pending = append(r.pending, m)
		r.recountLocked()
	}
	meta := handoff(ctx)
	meta.generation = r.generation
	if meta.done != nil {
		r.completions[m.DeliverySequence] = meta.done
		r.metadata[m.DeliverySequence] = meta
	}
	r.owner = "steer-recovery"
	r.mu.Unlock()
	return true, r.steerDecision(ctx, false)
}
func (r *codexIngress) recoveryDecision(ctx context.Context) (bool, error) {
	r.mu.Lock()
	if !r.recoveryBlockedLocked() {
		r.mu.Unlock()
		return false, nil
	}
	s := r.steer
	if r.closed || s == nil || s.working {
		if s != nil && s.working {
			s.again = true
		}
		r.mu.Unlock()
		return true, nil
	}
	s.working = true
	r.probes.Add(1)
	r.mu.Unlock()
	defer func() { r.mu.Lock(); s.working = false; r.mu.Unlock(); r.probes.Done() }()
	r.initSteerRecovery()
	r.mu.Lock()
	a := r.recovery
	generation := r.generation
	if a == nil || !r.recoveryBlockedLocked() {
		r.mu.Unlock()
		return false, nil
	}
	if a.unavailable || !a.loaded || a.record.Pending == nil {
		r.mu.Unlock()
		r.recoveryDiagnostic("steer-recovery-pending")
		return true, nil
	}
	record := a.record
	scan := a.scan
	r.mu.Unlock()
	if r.clearCompletedSteer(a) {
		return false, nil
	}
	if s.confirmed != nil && !s.confirmed() {
		r.recoveryDiagnostic("steer-recovery-pending")
		return true, nil
	}
	callCtx, cancel := steerScanContext(ctx)
	defer cancel()
	c, err := s.dial(callCtx)
	if err != nil {
		if errors.Is(err, codexcontrol.ErrNotDaemonHost) && r.clearSteerAttempt(a, "") {
			r.recoveryDiagnostic("steer-recovery-skipped")
			return false, nil
		}
		r.recoveryDiagnostic("steer-recovery-pending")
		return true, nil
	}
	defer c.Close()
	release, scanErr := advanceSteerScan(callCtx, c, a.thread, record, scan)
	if scan.ambiguous || scan.negative != nil && scan.negative.ambiguous {
		r.recoveryDiagnostic("steer-recovery-ambiguous")
	}
	r.mu.Lock()
	valid := r.recovery == a && generation == r.generation && !r.closed
	r.mu.Unlock()
	if !valid {
		return true, nil
	}
	// Complete only the recorded ordered prefix, using this generation's callbacks.
	through, err := a.store.LoadDeliveryWatermark(a.scope, a.registration, a.epoch)
	if err != nil {
		r.recoveryDiagnostic("steer-recovery-pending")
		return true, nil
	}
	for _, seq := range record.Pending.Sequences {
		if seq <= through {
			continue
		}
		r.mu.Lock()
		var message *broker.Message
		for i := range r.pending {
			if r.pending[i].DeliverySequence == seq {
				copy := r.pending[i]
				message = &copy
				break
			}
		}
		proof, found := scan.found[seq]
		meta := r.metadata[seq]
		valid = r.recovery == a && r.generation == generation && !r.closed && meta.generation == generation && meta.registration == a.registration && meta.epoch == a.epoch
		r.mu.Unlock()
		if !valid || message == nil || !found || !sameSteerEnvelope(*message, proof) || meta.done == nil {
			break
		}
		if meta.done() != nil {
			break
		}
		next, e := a.store.LoadDeliveryWatermark(a.scope, a.registration, a.epoch)
		if e != nil || next < seq {
			break
		}
		through = next
		r.mu.Lock()
		if r.recovery == a && r.generation == generation {
			for i, m := range r.pending {
				if m.DeliverySequence == seq {
					r.pending = append(r.pending[:i], r.pending[i+1:]...)
					break
				}
			}
			delete(r.metadata, seq)
			delete(r.completions, seq)
			r.recountLocked()
		}
		r.mu.Unlock()
		r.recoveryDiagnostic("steer-recovery-acked")
	}
	if r.clearCompletedSteer(a) {
		return false, nil
	}
	if scanErr == nil && release && r.clearSteerAttempt(a, "") {
		r.recoveryDiagnostic("steer-recovery-released")
		return false, nil
	}
	r.recoveryDiagnostic("steer-recovery-pending")
	return true, nil
}
