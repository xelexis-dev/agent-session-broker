package channel

import (
	"context"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"time"
)

const registrationInUseGuidance = "registration-in-use: 다른 client가 이 등록을 쓰고 있습니다. 상태 폴더를 복제했다면 그것이 원인일 수 있습니다."

func (o durableOptions) currentHolder(ctx context.Context) *broker.ReceiverHolder {
	if o.receiverIdentity == nil {
		return nil
	}
	return o.receiverIdentity.Current(ctx)
}
func holderBackoff(n int) time.Duration {
	return []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second}[min(max(n, 0), 3)]
}
func (o durableOptions) waitHolder(ctx context.Context, b *ingressBinding, c *broker.HolderChallenge) (*broker.HolderProof, error) {
	b.mu.Lock()
	first := !b.holderWaiting
	b.holderWaiting = true
	id := b.identity
	attempt := b.holderWaitAttempt
	b.holderWaitAttempt++
	b.mu.Unlock()
	if first {
		o.diagnose(b, "reconnect-failed", broker.CodeRegistrationInUse, "other")
	}
	o.requestCodexYieldForHolder(ctx, id, c)
	return o.waitHolderChallenge(ctx, c, holderBackoff(attempt), func() { o.diagnoseDeviceIdentity(b) })
}

func (o durableOptions) waitHolderChallenge(ctx context.Context, c *broker.HolderChallenge, remaining time.Duration, observe func()) (*broker.HolderProof, error) {
	sleep := o.resumeClock.sleep
	if sleep == nil {
		sleep = realPollClock().sleep
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sameDevice := false
		if c != nil && o.receiverIdentity != nil {
			probe, stop := context.WithTimeout(ctx, o.timeout)
			current := o.currentHolder(probe)
			sameDevice = current != nil && current.DeviceID == c.Holder.DeviceID
			dead := sameDevice && o.receiverIdentity.Dead(probe, *c)
			if observe != nil {
				observe()
			}
			stop()
			if dead {
				return &broker.HolderProof{Challenge: *c, Dead: true}, nil
			}
		}
		if remaining <= 0 {
			return nil, nil
		}
		delay := remaining
		if sameDevice {
			// Recheck local death without increasing the broker retry rate.
			delay = min(delay, time.Second)
		}
		if err := sleep(ctx, delay); err != nil {
			return nil, err
		}
		remaining -= delay
	}
}

func (o durableOptions) clearHolderWait(b *ingressBinding) {
	b.mu.Lock()
	b.holderWaiting = false
	b.holderWaitAttempt = 0
	b.mu.Unlock()
}
func (o durableOptions) exchangeHolder(ctx context.Context, b *ingressBinding, direct directAuthorizer, r directCodeRequest) (directGrant, error) {
	for {
		r.Holder = o.currentHolder(ctx)
		o.diagnoseDeviceIdentity(b)
		g, err := direct.exchange(ctx, r)
		var refusal broker.HandoverRefusal
		if !errors.As(err, &refusal) || refusal.Reason != broker.CodeRegistrationInUse {
			if err == nil {
				o.clearHolderWait(b)
			}
			return g, err
		}
		proof, waitErr := o.waitHolder(ctx, b, refusal.HolderChallenge)
		if waitErr != nil {
			return directGrant{}, waitErr
		}
		r.HolderProof = proof
	}
}

func (o durableOptions) exchangeAuthHolder(ctx context.Context, a *sessionAuth, id broker.AttachRequest, direct directAuthorizer, r directCodeRequest) (directGrant, error) {
	attempt := 0
	defer func() { a.mu.Lock(); a.holderWaiting = false; a.mu.Unlock() }()
	for {
		r.Holder = o.currentHolder(ctx)
		o.recordDeviceIdentity(a.key, id)
		g, err := direct.exchange(ctx, r)
		var refusal broker.HandoverRefusal
		if !errors.As(err, &refusal) || refusal.Reason != broker.CodeRegistrationInUse {
			return g, err
		}
		a.mu.Lock()
		first := !a.holderWaiting
		a.holderWaiting = true
		a.mu.Unlock()
		if first && o.state != nil {
			_ = o.state.RecordDiagnostic(a.key, clientstate.DiagnosticEvent{At: o.now().UTC(), SessionID: id.SessionID, RegistrationID: r.RegistrationID, Event: "reconnect-failed", Reason: broker.CodeRegistrationInUse, ErrorClass: "other"})
		}
		o.requestCodexYieldForHolder(ctx, id, refusal.HolderChallenge)
		proof, err := o.waitHolderChallenge(ctx, refusal.HolderChallenge, holderBackoff(attempt), func() { o.recordDeviceIdentity(a.key, id) })
		if err != nil {
			return directGrant{}, err
		}
		r.HolderProof = proof
		if proof == nil {
			attempt++
		}
	}
}
