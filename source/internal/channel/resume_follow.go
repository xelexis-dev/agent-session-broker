package channel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type followIdentityKey struct{}

var errFollowDelivery = errors.New("Claude conversation changed before delivery")

// mu serializes observations, transitions and publication of the current identity.
// Binding locks are never held while acquiring it or waiting for receivers.
type claudeFollow struct {
	mu           sync.Mutex
	initial      broker.AttachRequest
	current      broker.AttachRequest
	key          string
	opts         *durableOptions
	move         func(context.Context, broker.AttachRequest, broker.AttachRequest) (broker.AttachRequest, error)
	restore      func(context.Context, *ingressBinding) error
	startup      func(context.Context, broker.AttachRequest) (broker.AttachRequest, error)
	noRegistered func() bool
	retryID      string
	retryAt      time.Time
	lookup       func(string) *ingressBinding
	result       string
	began        time.Time
}

func newClaudeFollow(initial broker.AttachRequest, o *durableOptions) *claudeFollow {
	return &claudeFollow{initial: initial, key: o.handoverProcessKey(initial), opts: o, began: followPollClock(o).now()}
}
func (f *claudeFollow) eligible(b *ingressBinding) bool {
	if f.key == "" || b == nil || f.opts.device == nil {
		return false
	}
	b.mu.Lock()
	started := b.started
	a := b.durable
	detached := a != nil && a.detached
	b.mu.Unlock()
	if detached || !started {
		return false
	}
	if a == nil || a.session == nil {
		return false
	}
	a.session.mu.Lock()
	defer a.session.mu.Unlock()
	return a.session.state.LastAdmissionMode == "manual" || a.session.state.LastAdmissionMode == "auto" || (a.session.state.Credential != nil && a.session.state.Credential.AdmissionMode == "auto")
}
func followBindingStarted(b *ingressBinding) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.started
}

func (f *claudeFollow) check(ctx context.Context, fallback broker.AttachRequest) (broker.AttachRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.move == nil || f.lookup == nil {
		return fallback, nil
	}
	if f.current.SessionID == "" {
		f.current = fallback
	}
	old := f.current
	b := f.lookup(old.SessionID)
	// A later explicit attach may register a conversation after an unsaved
	// observation changed the monitor's last-seen identity.
	if !followBindingStarted(b) {
		if fallbackBinding := f.lookup(fallback.SessionID); followBindingStarted(fallbackBinding) {
			old = fallback
			f.current = fallback
			b = fallbackBinding
		}
	}
	eligible := f.eligible(b)
	startup := !eligible && f.startup != nil && f.noRegistered != nil && f.noRegistered() && f.key != "" && f.opts.device != nil
	if !eligible && !startup {
		return old, nil
	}
	explicitAttach, _ := ctx.Value(manualExplicitAttachKey{}).(bool)
	if !explicitAttach && f.retryID != "" && followPollClock(f.opts).now().Before(f.retryAt) && f.sessionFileMatches(f.retryID) {
		f.settle(ctx, b)
		return old, nil
	}
	if f.unchangedSession(old) {
		if f.retryID != old.SessionID {
			f.clearRetry()
		}
		f.restoreStopped(ctx, b)
		f.settle(ctx, b)
		return old, nil
	}
	current, err := f.opts.observe(ctx, f.initial)
	if err != nil {
		return old, nil
	} // roster failure is not a conversation change
	if current.Harness != "claude" || current.PID != f.initial.PID || !sameCWD(current.CWD, f.initial.CWD) || current.SessionID == "" || f.opts.handoverProcessKey(f.initial) != f.key {
		f.skipped(b, "parent-changed")
		return old, nil
	}
	if f.retryID != "" && f.retryID != current.SessionID {
		f.clearRetry()
	}
	if !explicitAttach && f.retryID == current.SessionID && followPollClock(f.opts).now().Before(f.retryAt) {
		f.settle(ctx, b)
		return old, nil
	}
	changed := current.SessionID != old.SessionID
	if !changed && (!startup || f.retryID != current.SessionID || followPollClock(f.opts).now().Before(f.retryAt)) {
		f.restoreStopped(ctx, b)
		f.settle(ctx, b)
		return old, nil
	}
	clock := f.opts.claudeStartupClock
	if clock.now == nil || clock.sleep == nil {
		clock = realPollClock()
	}
	// Require continuous stable observations, not simply two samples across a gap.
	began := clock.now()
	for clock.now().Sub(began) < 750*time.Millisecond {
		if err = clock.sleep(ctx, 100*time.Millisecond); err != nil {
			return old, err
		}
		next, e := f.opts.observe(ctx, f.initial)
		if e != nil {
			return old, nil
		}
		if next.Harness != current.Harness || next.SessionID != current.SessionID || next.PID != current.PID || !sameCWD(next.CWD, current.CWD) {
			return old, nil
		}
	}
	if f.opts.handoverProcessKey(f.initial) != f.key {
		f.skipped(b, "parent-changed")
		return old, nil
	}
	// The user already started approval for this target. Leave the old
	// binding in place while its poller completes; do not re-arm a refusal
	// gate or repeat approval-required diagnostics on every observation.
	if target := f.lookup(current.SessionID); deviceAuthorizationInFlight(target) {
		if explicitAttach {
			return current, nil
		}
		return old, nil
	}
	if !startup && f.manualApprovalRequired(b, current) {
		err := handoverError("approval-required")
		f.refusalRetry(ctx, current.SessionID, err)
		f.opts.diagnose(b, "handover-skipped", "approval-required", "")
		f.skipped(b, "approval-required")
		if explicitAttach {
			return current, nil
		}
		return old, nil
	}
	if startup {
		if changed {
			f.retryID = ""
			f.retryAt = time.Time{}
		}
		f.current = current // also selects the transcript hint for the next interval
		next, err := f.startup(ctx, current)
		if err != nil {
			f.refusalRetry(ctx, current.SessionID, err)
			f.skipped(f.lookup(current.SessionID), safeFollowReason(err))
			return current, nil
		}
		f.current = next
		f.retryID = ""
		f.retryAt = time.Time{}
		f.settle(ctx, f.lookup(next.SessionID))
		return next, nil
	}
	next, err := f.move(ctx, old, current)
	if err != nil {
		f.refusalRetry(ctx, current.SessionID, err)
		f.skipped(b, safeFollowReason(err))
		if explicitAttach && safeFollowReason(err) == "approval-required" {
			return current, nil
		}
		return old, nil
	}
	f.current = next
	f.clearRetry()
	if b != nil {
		b.mu.Lock()
		b.followStopped = false
		b.followRestoreAt = time.Time{}
		b.mu.Unlock()
		f.deliverPending(ctx, b, f.lookup(next.SessionID))
	}
	f.settle(ctx, f.lookup(next.SessionID))
	return next, nil
}
func safeFollowReason(err error) string {
	var refusal broker.HandoverRefusal
	if errors.As(err, &refusal) {
		if refusal.Reason == "detached" {
			return "detached"
		}
		return broker.SafeHandoverReason(refusal.Reason)
	}
	return "unavailable"
}
func (f *claudeFollow) skipped(b *ingressBinding, reason string) {
	f.result = "follow-skipped: " + reason
	if reason == "approval-required" {
		f.result += "; call attach_session when the user asks to connect, then approve keeping this registration"
	}
	if b != nil {
		b.mu.Lock()
		if b.receiveReason == "" || b.receiveGate == "follow" {
			b.receiveReason, b.receiveGate = "reconnect-required", "follow"
		}
		b.mu.Unlock()
	}
}

// Caller holds f.mu. Never use the unchanged-roster shortcut to release a send gate.
func (f *claudeFollow) settle(ctx context.Context, b *ingressBinding) {
	if b == nil || f.key == "" || f.opts.observe == nil {
		return
	}
	b.mu.Lock()
	owned := b.receiveGate == "follow" && b.receiveReason == "reconnect-required"
	generation, channel := b.generation, b.identity.ChannelID
	b.mu.Unlock()
	if !owned || (strings.HasPrefix(f.result, "follow-skipped: ") && f.result != "follow-skipped: unavailable") {
		return
	}
	if f.opts.handoverProcessKey(f.initial) != f.key {
		return
	}
	current, err := f.opts.observe(ctx, f.initial)
	if err != nil || f.opts.handoverProcessKey(f.initial) != f.key {
		return
	}
	if f.lookup != nil && f.lookup(current.SessionID) != b {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.receiveGate != "follow" || b.receiveReason != "reconnect-required" ||
		current.Harness != "claude" || current.Harness != b.identity.Harness ||
		current.SessionID != b.identity.SessionID || current.PID != b.identity.PID || !sameCWD(current.CWD, b.identity.CWD) ||
		current.PID != f.initial.PID || !sameCWD(current.CWD, f.initial.CWD) ||
		b.generation != generation || b.identity.ChannelID != channel || channel == "" ||
		!b.ingressReady || b.ingressReadyChannel != channel || b.ingressReadyGeneration != generation ||
		!b.started || b.failed || b.retired || b.receiveUnavailable || b.followStopped || b.sink == nil || b.done == nil ||
		b.resumeStopReason != "" || b.resumeSuspended || b.holderWaiting || f.opts.ingressPending() ||
		receiveDenied(b.upstream) || upstreamSuspended(b.upstream) || projectTerminalUpstream(b.upstream) != nil ||
		(b.durable != nil && b.durable.detached) {
		return
	}
	select {
	case <-b.done:
		return
	default:
	}
	b.receiveReason, b.receiveGate = "", ""
	f.result = "followed: resumed"
}

func (f *claudeFollow) summary() string { f.mu.Lock(); defer f.mu.Unlock(); return f.result }

// Called outside any receiver. The retired receiver is joined before a new
// receiver starts. Failed handovers never fall back to admission/register.
func (f *claudeFollow) install(ctx context.Context, o durableOptions, mu *sync.Mutex, workers *sync.WaitGroup, stopping *bool, bindings map[string]*ingressBinding, prepare func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error), register registerFunc, attached func(broker.AttachRequest, broker.Session) error, ensure func(*ingressBinding) Upstream) {
	f.lookup = func(id string) *ingressBinding { mu.Lock(); defer mu.Unlock(); return bindings[id] }
	f.noRegistered = func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, b := range bindings {
			if b.started {
				return false
			}
		}
		return true
	}
	f.move = func(callCtx context.Context, oldID, id broker.AttachRequest) (broker.AttachRequest, error) {
		old := f.lookup(oldID.SessionID)
		if old == nil {
			return oldID, errors.New("old binding unavailable")
		}
		mu.Lock()
		target := old.upstream
		oldReg := old.durable.registrationID
		oldName := old.durable.name
		oldKey := old.durable.key
		mu.Unlock()
		cid, err := o.state.ClientID()
		if err != nil {
			return oldID, err
		}
		saved, found, err := o.state.LoadSession(clientstate.SessionKey(o.scope, id.Harness, id.SessionID, cid))
		if err != nil {
			return oldID, err
		}
		savedTarget := found && saved.Registration.RegistrationID != ""
		if found && (saved.Registration.RegistrationID != "" || saved.Registration.HandedOverTo != "") {
			holder, e := o.state.HandoverHolder(clientstate.SessionKey(o.scope, id.Harness, id.SessionID, cid), o.scope, id.Harness, cid, false)
			if e != nil {
				return oldID, broker.HandoverRefusal{Reason: handoverResolutionReason(e)}
			}
			savedTarget = holder.Key == clientstate.SessionKey(o.scope, id.Harness, id.SessionID, cid)
		}
		if savedTarget {
			// An earlier call may have completed handover but been canceled
			// before resume. Its source credential is already revoked; inspect
			// through the persisted destination credential on that retry.
			if saved.Registration.RegistrationID == oldReg && saved.Registration.HandoverFrom != nil {
				if pending := f.lookup(id.SessionID); pending != nil {
					target = ensure(pending)
				} else if saved.Credential != nil {
					// Marker writes may have failed before the destination binding was
					// published. Inspect through its committed credential, not the revoked source.
					probe := o.device.newUpstream(saved.Credential.AccessToken)
					defer closeUpstream(probe)
					target = probe
				}
			}
			if saved.Registration.Detached {
				return oldID, broker.HandoverRefusal{Reason: "detached"}
			}
			var remote broker.Session
			if target == nil || call(callCtx, target, "get_session", map[string]string{"name": broker.SelectorRegistration + saved.Registration.RegistrationID}, &remote) != nil {
				return oldID, errors.New("target unavailable")
			}
			switch {
			case remote.Suspended:
				return oldID, broker.HandoverRefusal{Reason: "suspended"}
			case remote.ErrorCode == broker.CodeDetached:
				return oldID, broker.HandoverRefusal{Reason: "detached"}
			case remote.Status == broker.StatusAlive && remote.PID != id.PID:
				return oldID, broker.HandoverRefusal{Reason: "still-receiving"}
			}
		}
		if !savedTarget && old.durable.session != nil {
			old.durable.session.mu.Lock()
			manual := old.durable.session.state.LastAdmissionMode == "manual"
			old.durable.session.mu.Unlock()
			if manual {
				o.diagnose(old, "handover-skipped", "approval-required", "")
				return oldID, handoverError("approval-required")
			}
		}
		callCtx = context.WithValue(callCtx, followIdentityKey{}, id)
		nextID, err := prepare(callCtx, nil)
		if err != nil {
			return oldID, err
		}
		next := f.lookup(id.SessionID)
		if !savedTarget {
			// Explicit source selection prevents choosing another local process or
			// falling through the explicit handler's fresh-registration fallback.
			mu.Lock()
			valid := old.durable.parentPID == id.PID && old.durable.processKey == f.key
			mu.Unlock()
			if !valid {
				return oldID, broker.HandoverRefusal{Reason: "refused"}
			}
			mu.Lock()
			old.retired, old.selfStopped = true, false
			mu.Unlock()
			if err = o.prepareConversationHandover(context.WithValue(context.WithValue(callCtx, takeoverContextKey{}, oldName), handoverSourceContextKey{}, oldKey), next, nextID); err != nil {
				mu.Lock()
				old.retired = false
				mu.Unlock()
				return oldID, err
			}
			mu.Lock()
			same := next.durable.registrationID == oldReg
			mu.Unlock()
			if !same {
				return oldID, broker.HandoverRefusal{Reason: "refused"}
			}
		}
		session, err := register(callCtx, nextID)
		if err != nil {
			return oldID, err
		}
		// Stop the old lease and prevent credential recovery from reviving it.
		mu.Lock()
		old.retired, old.selfStopped = true, false
		cancel, done := old.cancel, old.done
		mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if done != nil {
			select {
			case <-done:
			case <-callCtx.Done():
				return oldID, callCtx.Err()
			}
		}
		mu.Lock()
		nextID = next.identity
		temporary := old.temporary && !old.used
		mu.Unlock()
		old.mu.Lock()
		heldAck := old.followAck != nil
		old.mu.Unlock()
		if heldAck {
			f.deliverPending(callCtx, old, next)
		}
		if err = attached(nextID, session); err != nil {
			return oldID, err
		}
		if savedTarget && temporary && target != nil {
			// This uses A's credential, hence remains a self detach even for workers.
			if err = call(callCtx, target, "stop_session", map[string]string{"name": broker.SelectorRegistration + oldReg}, nil); err == nil {
				o.detachedObserved(old)
			}
		}
		mu.Lock()
		if old.unlock != nil {
			old.unlock()
			old.unlock = nil
		}
		closeUpstream(old.upstream)
		old.upstream = nil
		delete(bindings, oldID.SessionID)
		mu.Unlock()
		if savedTarget {
			f.result = "followed: resumed"
		} else {
			f.result = "followed: handover"
		}
		return nextID, nil
	}
	f.restore = func(callCtx context.Context, b *ingressBinding) error {
		mu.Lock()
		if *stopping || ctx.Err() != nil || bindings[b.identity.SessionID] != b || b.retired {
			mu.Unlock()
			return errors.New("source receiver is unavailable")
		}
		target := b.upstream
		cancel, done := b.cancel, b.done
		mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if done != nil {
			select {
			case <-done:
			case <-callCtx.Done():
				return callCtx.Err()
			}
		}
		if target == nil {
			return errors.New("source upstream is unavailable")
		}
		// Resume after the previous receiver and its heartbeat have exited.
		// A fresh lease prevents an old wait from competing with this receiver.
		b.mu.Lock()
		b.receiveUnavailable = false
		b.mu.Unlock()
		session, ok := resumeDurable(callCtx, target, b, o, "")
		if !ok {
			return errors.New("source receiver could not resume")
		}
		mu.Lock()
		b.started, b.failed = false, false
		b.receiveReason, b.receiveGate = "", ""
		id := b.identity
		heldAck := b.followAck != nil
		mu.Unlock()
		if heldAck && !f.deliverPending(callCtx, b, b) {
			return errors.New("held inbox handoff failed")
		}
		return attached(id, session)
	}
	// The monitor shares check's serializer with tool calls and delivery.
	mu.Lock()
	if *stopping || ctx.Err() != nil {
		mu.Unlock()
		return
	}
	workers.Add(1)
	mu.Unlock()
	go func() {
		defer workers.Done()
		for {
			f.mu.Lock()
			id := f.current
			if id.SessionID == "" {
				id = f.initial
			}
			interval := f.interval(followPollClock(f.opts).now(), id)
			f.mu.Unlock()
			if followPollClock(f.opts).sleep(ctx, interval) != nil {
				return
			}
			checkCtx, cancel := context.WithTimeout(ctx, 2*o.timeout)
			_, _ = f.check(checkCtx, id)
			cancel()
		}
	}()
}
func (f *claudeFollow) interval(now time.Time, id broker.AttachRequest) time.Duration {
	if now.Sub(f.began) < 3*time.Minute || !claudeRecordExists(id) {
		return 2 * time.Second
	}
	return 30 * time.Second
}
func claudeRecordExists(id broker.AttachRequest) bool {
	root := os.Getenv("CLAUDE_CONFIG_DIR")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		root = filepath.Join(home, ".claude")
	}
	// Only stat the filename. No transcript contents are read.
	project := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, id.CWD)
	_, err := os.Stat(filepath.Join(root, "projects", project, id.SessionID+".jsonl"))
	return err == nil
}
func (f *claudeFollow) wrapSink(b *ingressBinding, id broker.AttachRequest, sink func(context.Context, broker.Message) error) func(context.Context, broker.Message) error {
	return func(ctx context.Context, m broker.Message) error {
		// A transition may hold f.mu while joining this receiver or
		// redelivering through its sink. Never wait for it in a receiver.
		unchanged := false
		if f.mu.TryLock() {
			unchanged = f.unchangedSession(id)
			f.mu.Unlock()
		}
		current := id
		var err error
		if !unchanged {
			current, err = f.opts.observe(ctx, f.initial)
		}
		if err == nil && current.SessionID != "" && current.SessionID != id.SessionID && f.eligible(b) {
			b.mu.Lock()
			copy := m
			b.followMessage = &copy
			b.followAck = handoff(ctx).follow
			b.mu.Unlock()
			return errFollowDelivery
		}
		if err = sink(ctx, m); err == nil {
			b.mu.Lock()
			b.used = true
			b.mu.Unlock()
		}
		return err
	}
}

// Transfer ownership of the pending envelope only after the sink accepts it.
// Both source fallback and destination delivery use the same failure path.
func (f *claudeFollow) deliverPending(ctx context.Context, source, target *ingressBinding) bool {
	source.mu.Lock()
	message := source.followMessage
	complete := source.followAck
	source.mu.Unlock()
	if message == nil {
		return true
	}
	if target == nil {
		f.deliveryFailed(source, errors.New("destination binding is unavailable"))
		return false
	}
	target.mu.Lock()
	sink := target.sink
	target.mu.Unlock()
	var err error
	if sink == nil {
		err = errors.New("destination inbox is unavailable")
	} else {
		deliveryCtx := ctx
		if complete != nil {
			deliveryCtx = context.WithValue(ctx, handoffKey{}, handoffCompletion{follow: complete})
		}
		err = sink(deliveryCtx, *message)
	}
	if err != nil {
		f.deliveryFailed(target, err)
		return false
	}
	if complete != nil {
		if err := complete(ctx); err != nil {
			f.deliveryFailed(target, err)
			return false
		}
	}
	source.mu.Lock()
	if source.followMessage == message {
		source.followMessage = nil
		source.followAck = nil
	}
	source.mu.Unlock()
	return true
}
func (f *claudeFollow) deliveryFailed(b *ingressBinding, err error) {
	b.mu.Lock()
	b.receiveUnavailable = true
	b.receiveReason, b.receiveGate = "reconnect-required", ""
	b.followStopped = true
	target, id := b.upstream, b.identity
	registration := ""
	if b.durable != nil {
		registration = b.durable.registrationID
	}
	b.mu.Unlock()
	class := diagnosticErrorClass(err, false)
	f.opts.diagnose(b, "sink-failed", class, class)
	if target != nil {
		reportIngressFailure(target, id, registration)
	}
}
func (f *claudeFollow) redeliver(ctx context.Context, old broker.AttachRequest, _ broker.Message) {
	id, err := f.check(ctx, old)
	if ctx.Err() != nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if id.SessionID != old.SessionID {
		return
	} // Successful check already delivered to the destination.
	source := f.lookup(old.SessionID)
	if source == nil {
		return
	}
	if err != nil {
		f.deliveryFailed(source, err)
		return
	}
	source.mu.Lock()
	pending, stopped := source.followMessage != nil, source.followStopped
	source.mu.Unlock()
	if !pending || stopped {
		return
	}
	if !f.deliverPending(ctx, source, source) {
		return
	}
	if f.restore == nil {
		f.deliveryFailed(source, errors.New("source receiver is unavailable"))
		return
	}
	if err = f.restore(ctx, source); err != nil {
		f.deliveryFailed(source, err)
	}
}

func followPollClock(o *durableOptions) devicePollClock {
	if o.claudeFollowClock.now != nil && o.claudeFollowClock.sleep != nil {
		return o.claudeFollowClock
	}
	return realPollClock()
}
