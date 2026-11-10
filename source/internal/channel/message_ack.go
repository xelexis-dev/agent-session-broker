package channel

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type messageAckUpstream interface{ verifyMessageAck(context.Context) bool }
type handoffKey struct{}
type handoffCompletion struct {
	registration, epoch string
	generation          uint64
	done                func() error
	follow              func(context.Context) error
}

func handoff(ctx context.Context) handoffCompletion {
	v, _ := ctx.Value(handoffKey{}).(handoffCompletion)
	return v
}

type messageAckState struct {
	mu                                sync.Mutex
	store                             *clientstate.Store
	scope, registration, epoch, lease string
	through, cursor                   uint64
	issued                            []uint64
	completed                         map[uint64]bool
	wake                              chan struct{}
	failed                            chan struct{}
	failOnce                          sync.Once
}

func (o durableOptions) deliveryCapabilities(ctx context.Context, up Upstream, harness string) []string {
	out := []string{broker.MessageSentAtCapability}
	if o.device != nil {
		if _, ok := o.device.authorizer.(directAuthorizer); ok {
			out = append(out, broker.ReattachAfterCleanupCapability)
		}
	}
	capable, ok := up.(messageAckUpstream)
	if !ok || !capable.verifyMessageAck(ctx) {
		return out
	}
	ready := harness == "claude" || (harness == "codex" && o.hookWrites != nil)
	if harness == "kimi" && o.kimi != nil {
		o.kimi.mu.Lock()
		ready = o.kimi.helperVersion >= 2
		o.kimi.mu.Unlock()
	}
	if ready {
		out = append(out, broker.MessageAckCapability)
		if harness == "claude" || harness == "codex" {
			out = append(out, broker.AttachmentCapability)
		}
	}
	return out
}
func (o durableOptions) negotiateMessageAck(b *ingressBinding, result broker.RegisterResult, lease string, capabilities []string) error {
	old := bindingMessageAck(b)
	sameLease := old != nil && old.registration == result.Session.RegistrationID && old.epoch == result.Session.Epoch && old.lease == lease
	if sameLease {
		if result.MessageAckVersion != 1 {
			return errors.New("negotiated acknowledgement changed on the active lease")
		}
		return nil
	}
	if result.MessageAckVersion == 0 {
		b.mu.Lock()
		b.messageAck = nil
		b.mu.Unlock()
		return nil
	}
	if result.MessageAckVersion != 1 || !broker.SupportsMessageAck(capabilities) {
		return errors.New("unsupported acknowledgement negotiation")
	}

	if o.state == nil {
		return errors.New("delivery metadata store is unavailable")
	}
	through, err := o.state.LoadDeliveryWatermark(o.scope, result.Session.RegistrationID, result.Session.Epoch)
	if err != nil {
		return err
	}
	ack := &messageAckState{store: o.state, scope: o.scope, registration: result.Session.RegistrationID, epoch: result.Session.Epoch, lease: lease, through: through, completed: map[uint64]bool{}, wake: make(chan struct{}, 1), failed: make(chan struct{})}
	b.mu.Lock()
	b.messageAck = ack
	b.mu.Unlock()
	if old != nil && old.scope == ack.scope && old.registration == ack.registration && old.epoch != ack.epoch {
		_ = old.store.DeleteDeliveryWatermark(old.scope, old.registration, old.epoch)
	}
	return nil
}
func (s *messageAckState) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *messageAckState) refresh() error {
	through, err := s.store.LoadDeliveryWatermark(s.scope, s.registration, s.epoch)
	if err == nil && through > s.through {
		s.through = through
	}
	return err
}
func (s *messageAckState) observe(sequence uint64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sequence == 0 {
		return false, errors.New("ack receiver requires a delivery sequence")
	}
	if err := s.refresh(); err != nil {
		return false, err
	}
	if sequence > s.cursor {
		s.cursor = sequence
	}
	if sequence <= s.through {
		s.signal()
		return true, nil
	}
	if slices.Contains(s.issued, sequence) {
		return true, nil
	}
	s.issued = append(s.issued, sequence)
	return false, nil
}
func (s *messageAckState) complete(sequence uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sequence <= s.through {
		s.signal()
		return nil
	}
	s.completed[sequence] = true
	n := 0
	through := s.through
	for n < len(s.issued) && s.completed[s.issued[n]] {
		through = s.issued[n]
		n++
	}
	if n == 0 {
		return nil
	}
	if err := s.store.SaveDeliveryWatermark(s.scope, s.registration, s.epoch, through); err != nil {
		s.failOnce.Do(func() { close(s.failed) })
		return err
	}
	s.through = through
	for _, seq := range s.issued[:n] {
		delete(s.completed, seq)
	}
	s.issued = s.issued[n:]
	s.signal()
	return nil
}
func (s *messageAckState) snapshot() (uint64, uint64) {
	if s == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return min(s.through, s.cursor), s.cursor
}
func bindingMessageAck(b *ingressBinding) *messageAckState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.messageAck
}
func renewMessageAck(r broker.RenewRequest, s *messageAckState) broker.RenewRequest {
	if s != nil {
		r.Epoch = s.epoch
		r.AckThrough, _ = s.snapshot()
	}
	return r
}
func (s *messageAckState) completion(ctx context.Context, b *ingressBinding, sequence uint64) handoffCompletion {
	return handoffCompletion{
		registration: s.registration, epoch: s.epoch,
		done: func() error {
			b.mu.Lock()
			valid := ctx.Err() == nil && !b.retired && !b.failed && b.messageAck == s && b.identity.ChannelID == s.lease
			b.mu.Unlock()
			if !valid {
				return context.Canceled
			}
			return s.complete(sequence)
		},
		// A follow coordinator has joined the fenced source and performed a new
		// target Post. Persist completion; only the current lease may transmit ack.
		follow: func(targetCtx context.Context) error {
			if targetCtx.Err() != nil {
				return targetCtx.Err()
			}
			return s.complete(sequence)
		},
	}
}

// Not-found is terminal for an immutable registration ID; detached can be
// cleared by same-epoch reattach, so it must never discard a dedupe floor.
func (o durableOptions) pruneMessageAck(b *ingressBinding, registration, code string) {
	if code != broker.CodeNotFound {
		return
	}
	ack := bindingMessageAck(b)
	if ack != nil && ack.scope == o.scope && ack.registration == registration {
		_ = ack.store.DeleteDeliveryWatermark(ack.scope, ack.registration, ack.epoch)
	}
}
