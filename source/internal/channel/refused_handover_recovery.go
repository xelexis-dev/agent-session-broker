package channel

import (
	"context"
	"errors"
	"time"
)

// All retry and recovery decisions run under the follow serializer, outside
// receivers. A receiver may need that serializer after publishing its exit.
func (f *claudeFollow) clearRetry() { f.retryID = ""; f.retryAt = time.Time{} }
func (f *claudeFollow) refusalRetry(ctx context.Context, id string, err error) {
	// A cancelled tool may already have committed a handover. Let the next
	// check resume its persisted result immediately rather than treating it as refusal.
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return
	}
	delay := 30 * time.Second
	if safeFollowReason(err) == "unsupported-mode" {
		delay = 10 * time.Minute
	}
	f.retryID = id
	f.retryAt = followPollClock(f.opts).now().Add(delay)
}
func (f *claudeFollow) restoreStopped(ctx context.Context, b *ingressBinding) {
	if b == nil {
		return
	}
	b.mu.Lock()
	stopped, at := b.followStopped, b.followRestoreAt
	b.mu.Unlock()
	if !stopped || followPollClock(f.opts).now().Before(at) {
		return
	}
	var err error
	if f.restore == nil {
		err = errors.New("source receiver is unavailable")
	} else {
		err = f.restore(ctx, b)
	}
	if err == nil && f.deliverPending(ctx, b, b) {
		b.mu.Lock()
		b.followStopped = false
		b.followRestoreAt = time.Time{}
		b.receiveReason, b.receiveGate = "", ""
		b.mu.Unlock()
		f.result = "followed: resumed"
		return
	}
	if err != nil {
		f.deliveryFailed(b, err)
		class := diagnosticErrorClass(err, false)
		f.opts.diagnose(b, "reconnect-failed", class, class)
	}
	b.mu.Lock()
	b.followRestoreAt = followPollClock(f.opts).now().Add(30 * time.Second)
	b.mu.Unlock()
}
