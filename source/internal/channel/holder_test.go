package channel

import (
	"context"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"strings"
	"sync"
	"testing"
	"time"
)

// In-memory fixtures multiplex simulated clients inside this Go process.
// Their default receiver is one incarnation; guard tests override it explicitly.
func fixtureHolder() *broker.ReceiverHolder {
	return &broker.ReceiverHolder{PID: 1, DeviceID: strings.Repeat("f", 64), Incarnation: strings.Repeat("e", 64)}
}

type fixtureReceiverIdentity struct {
	holder      *broker.ReceiverHolder
	dead        bool
	unavailable bool
}

func (f fixtureReceiverIdentity) Current(context.Context) *broker.ReceiverHolder {
	if f.unavailable {
		return nil
	}
	if f.holder != nil {
		copy := *f.holder
		return &copy
	}
	return fixtureHolder()
}
func (f fixtureReceiverIdentity) Dead(context.Context, broker.HolderChallenge) bool { return f.dead }
func TestHolderWaitScheduleCancellationAndDiagnosticBound(t *testing.T) {
	b := &ingressBinding{mu: &sync.Mutex{}, durable: &durableRegistration{}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var waits []time.Duration
	o := durableOptions{timeout: time.Second, resumeClock: devicePollClock{sleep: func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		if len(waits) == 6 {
			cancel()
		}
		return ctx.Err()
	}}}
	for i := 0; i < 6; i++ {
		_, err := o.waitHolder(ctx, b, nil)
		if (err != nil) != (i == 5) {
			t.Fatal("wait cancellation changed")
		}
	}
	expected := []time.Duration{5, 10, 20, 30, 30, 30}
	for i, v := range expected {
		if waits[i] != v*time.Second {
			t.Fatal("unbounded or wrong holder retry schedule")
		}
	}
	if !b.holderWaiting {
		t.Fatal("holder wait became permanent failure")
	}
	o.clearHolderWait(b)
	if b.holderWaiting || b.holderWaitAttempt != 0 {
		t.Fatal("holder wait did not clear")
	}
}
func TestHolderWaitDeadChallengeDoesNotSleep(t *testing.T) {
	b := &ingressBinding{mu: &sync.Mutex{}, durable: &durableRegistration{}}
	o := durableOptions{timeout: time.Second, receiverIdentity: fixtureReceiverIdentity{dead: true}, resumeClock: devicePollClock{sleep: func(context.Context, time.Duration) error { t.Fatal("dead-holder proof slept"); return nil }}}
	c := &broker.HolderChallenge{Holder: *fixtureHolder(), Revision: 4, Generation: 5, Nonce: "synthetic"}
	proof, err := o.waitHolder(t.Context(), b, c)
	if err != nil || proof == nil || !proof.Dead || proof.Challenge != *c {
		t.Fatal("proof was not bound to exact challenge")
	}
}

func TestHolderUnknownCurrentProcessCannotUseDeathProof(t *testing.T) {
	b := &ingressBinding{mu: &sync.Mutex{}, durable: &durableRegistration{}}
	sleeps := 0
	o := durableOptions{timeout: time.Second, receiverIdentity: fixtureReceiverIdentity{dead: true, unavailable: true}, resumeClock: devicePollClock{sleep: func(context.Context, time.Duration) error { sleeps++; return nil }}}
	proof, err := o.waitHolder(t.Context(), b, &broker.HolderChallenge{Holder: *fixtureHolder()})
	if err != nil || proof != nil || sleeps != 1 {
		t.Fatal("unknown receiver used a death exception")
	}
}
