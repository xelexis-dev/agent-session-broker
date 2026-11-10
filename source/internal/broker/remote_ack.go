package broker

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

const ackWindowMessages = 32
const ackWindowBytes = 64 * 1024

var errMessageAck = errors.New("invalid message acknowledgement or cursor")

const ackDowngradeWarning = "Unacknowledged delivery is protected for up to ten minutes; retry registration with backoff."

func (e *remoteSession) ackVersion() int {
	if e.messageAck {
		return 1
	}
	return 0
}
func (e *remoteSession) requeueInflight() {
	if len(e.inflight) != 0 {
		e.queue = append(slices.Clone(e.inflight), e.queue...)
		e.inflight = nil
		e.inflightBytes = 0
	}
	e.issuedThrough, e.leaseAckThrough = 0, 0
}
func (e *remoteSession) protectAckLoss(at time.Time) {
	if len(e.unacked) != 0 && e.ackProtectedUntil.IsZero() {
		e.ackProtectedUntil = at.Add(expiredIngressQueueRetention)
	}
}
func (e *remoteSession) discardMessages() {
	if e.attachments != nil {
		for _, messages := range [][]Message{e.queue, e.inflight} {
			for _, m := range messages {
				if m.Attachment != nil {
					e.attachments.Delete(m.Attachment.ID, "discard")
				}
			}
		}
	}
	e.queue, e.inflight = nil, nil
	e.unacked = nil
	e.inflightBytes = 0
	e.issuedThrough, e.leaseAckThrough = 0, 0
	e.queueHeldUntil, e.ackProtectedUntil = time.Time{}, time.Time{}
}

// Protection expiry releases the downgrade fence, not the retained payload.
func (e *remoteSession) releaseAckProtection(now time.Time) bool {
	if len(e.unacked) == 0 || e.ackProtectedUntil.IsZero() || now.Before(e.ackProtectedUntil) {
		return false
	}
	e.requeueInflight()
	e.unacked = nil
	e.queueHeldUntil = time.Time{}
	return true
}
func (s *RemoteService) ackDowngradeBlocked(e *remoteSession, capabilities []string) bool {
	if protocol.SupportsMessageAck(capabilities) || len(e.unacked) == 0 {
		return false
	}
	e.protectAckLoss(s.now())
	return !e.releaseAckProtection(s.now())
}

// Both validation steps precede mutation so an invalid cursor cannot commit an ack.
func (e *remoteSession) validateAck(epoch string, through, cursor uint64, wait bool) (int, error) {
	invalid := errMessageAck
	if !e.messageAck {
		if through != 0 || cursor != 0 || epoch != "" {
			return 0, invalid
		}
		return 0, nil
	}
	if (through != 0 || epoch != "") && epoch != e.session.Epoch {
		return 0, invalid
	}
	if wait && cursor > e.issuedThrough {
		return 0, invalid
	}
	if through == 0 || through <= e.leaseAckThrough {
		return 0, nil
	}
	for i, m := range e.inflight {
		if m.DeliverySequence == through {
			return i + 1, nil
		}
	}
	return 0, invalid
}
func (s *RemoteService) applyAck(e *remoteSession, n int) {
	if n == 0 {
		return
	}
	for _, m := range e.inflight[:n] {
		if s.attachments != nil && m.Attachment != nil {
			s.attachments.Delete(m.Attachment.ID, "ack")
		}
		delete(e.unacked, m.DeliverySequence)
		data, _ := json.Marshal(m)
		e.inflightBytes -= len(data)
		if m.DeliverySequence == e.ackedThrough+1 {
			e.ackedThrough = m.DeliverySequence
		}
		e.leaseAckThrough = m.DeliverySequence
	}
	copy(e.inflight, e.inflight[n:])
	clear(e.inflight[len(e.inflight)-n:])
	e.inflight = e.inflight[:len(e.inflight)-n]
	s.notify(e)
}
func (e *remoteSession) nextAckMessage(cursor uint64) (Message, bool) {
	for _, m := range e.inflight {
		if m.DeliverySequence > cursor {
			return m, true
		}
	}
	if len(e.queue) == 0 || len(e.inflight) >= ackWindowMessages {
		return Message{}, false
	}
	m := e.queue[0]
	data, _ := json.Marshal(m)
	if e.inflightBytes+len(data) > ackWindowBytes {
		return Message{}, false
	}
	e.queue[0] = Message{}
	e.queue = e.queue[1:]
	e.inflight = append(e.inflight, m)
	e.inflightBytes += len(data)
	if e.unacked == nil {
		e.unacked = map[uint64]bool{}
	}
	e.unacked[m.DeliverySequence] = true
	e.issuedThrough = m.DeliverySequence
	return m, true
}

// WaitForIngressAck is the wire receiver. The legacy Go entry point shares its loop.
func (s *RemoteService) WaitForIngressAck(ctx context.Context, r protocol.IngressRequest) (Message, error) {
	return s.waitForIngress(ctx, r, true)
}
