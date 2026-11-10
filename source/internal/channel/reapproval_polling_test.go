package channel

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
)

type pendingPollSleep struct {
	duration time.Duration
	release  chan struct{}
}

// The authorization boundary enforces the store's interval/slow_down rule.
// Time moves only when the test releases a named sleeper; no wall-clock pacing.
type pacedApproval struct {
	mu                 sync.Mutex
	now                time.Time
	next               time.Time
	interval           time.Duration
	codes, polls, slow int
	approved           bool
	sleeps             chan pendingPollSleep
}

func (p *pacedApproval) requestCode(context.Context, deviceCodeRequest) (deviceAuthorization, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.codes++
	p.polls = 0
	p.slow = 0
	p.next = time.Time{}
	p.interval = 5 * time.Second
	return deviceAuthorization{DeviceCode: fmt.Sprintf("synthetic-code-%d", p.codes), UserCode: "SYNTHETIC", VerificationURI: "/auth/code", ExpiresIn: 600, Interval: 5}, nil
}
func (p *pacedApproval) redeem(context.Context, string) (sessionGrant, tokenOutcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.polls++
	if p.now.Before(p.next) {
		p.interval += 5 * time.Second
		p.next = p.now.Add(p.interval)
		p.slow++
		return sessionGrant{}, tokenSlowDown, nil
	}
	p.next = p.now.Add(p.interval)
	if !p.approved {
		return sessionGrant{}, tokenPending, nil
	}
	credential := "asb_s_old"
	if p.codes > 1 {
		credential = "asb_s_new"
	}
	return sessionGrant{AccessToken: credential, TokenType: "Bearer", ExpiresIn: 3600, SessionGrantID: "synthetic-grant", Nickname: "agent-one"}, tokenGranted, nil
}
func (p *pacedApproval) clock() devicePollClock {
	return devicePollClock{now: func() time.Time { p.mu.Lock(); defer p.mu.Unlock(); return p.now }, sleep: func(ctx context.Context, d time.Duration) error {
		sleeper := pendingPollSleep{d, make(chan struct{})}
		select {
		case p.sleeps <- sleeper:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-sleeper.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
}
func (p *pacedApproval) advance(s pendingPollSleep) {
	p.mu.Lock()
	p.now = p.now.Add(s.duration)
	p.mu.Unlock()
	close(s.release)
}

// Replacing a retired receiver must not duplicate the pending device poller.
// A second poller at the same virtual time escalates slow_down and strands
// the approved code; preserving one conversation's poll ownership prevents it.
func TestReapprovalKeepsSinglePollerAcrossBindingReplacement(t *testing.T) {
	p := &pacedApproval{now: time.Now(), approved: true, sleeps: make(chan pendingPollSleep, 8)}
	rejected, releaseOld, redeemed := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	var releaseOnce, rejectOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(releaseOld) }) }
	defer unblock()
	tracker := &reapprovalDiagnostics{}
	f := newDeviceModeFixture(t, newDeviceAuthServer(t), newSwitchableUpstream(t), t.TempDir(), singleIdentity(), func(f *deviceModeFixture) {
		f.configureDurable = func(o *durableOptions) {
			o.bindingCreated = tracker.bindingCreated
			o.device.authorizer = p
			o.device.clock = p.clock()
		}
		f.persist = func(s *clientstate.Store, key string, state clientstate.SessionState) error {
			err := s.SaveSession(key, state)
			if state.Device != nil && state.Device.Status == clientstate.DeviceRejected {
				rejectOnce.Do(func() { close(rejected); <-releaseOld })
			}
			if state.Credential != nil && state.Credential.AccessToken == "asb_s_new" {
				select {
				case redeemed <- struct{}{}:
				default:
				}
			}
			return err
		}
	})
	t.Cleanup(unblock)
	waitFor(t, "initial receiver", func() bool {
		tracker.mu.Lock()
		b := tracker.current
		tracker.mu.Unlock()
		if b == nil {
			return false
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.started
	})
	tracker.mu.Lock()
	old := tracker.current
	tracker.mu.Unlock()
	old.mu.Lock()
	oldDone, oldAuth := old.done, old.durable.session
	old.mu.Unlock()
	original, _ := f.broker.session("agent-one")
	p.mu.Lock()
	p.approved = false
	p.mu.Unlock()
	f.mu.Lock()
	f.upstreams[0].rejected.Store(true)
	f.mu.Unlock()
	select {
	case <-rejected:
	case <-time.After(3 * time.Second):
		t.Fatal("rejection barrier not reached")
	}
	if r := f.call("", "attach_session", map[string]any{}); !r.IsError {
		t.Fatal("replacement approval not requested")
	}
	var first pendingPollSleep
	select {
	case first = <-p.sleeps:
	case <-time.After(3 * time.Second):
		t.Fatal("replacement poll did not wait")
	}
	unblock()
	select {
	case <-oldDone:
	case <-time.After(3 * time.Second):
		t.Fatal("retired receiver did not finish")
	}
	// This ordinary call recreates the binding while its original poller sleeps.
	if s := f.status(""); s["status"] != "pending" {
		t.Fatal("approval was not pending before browser approval")
	}
	tracker.mu.Lock()
	current := tracker.current
	tracker.mu.Unlock()
	current.mu.Lock()
	currentAuth := current.durable.session
	current.mu.Unlock()
	if currentAuth != oldAuth {
		// Drain the additional worker's first sleep as a causal barrier on the
		// broken implementation. Its early poll must precede the browser approval.
		select {
		case <-p.sleeps:
		case <-time.After(3 * time.Second):
			t.Fatal("replacement worker did not reach the polling boundary")
		}
	}
	p.mu.Lock()
	p.approved = true
	p.mu.Unlock()
	p.advance(first)
	select {
	case <-redeemed:
	case <-p.sleeps:
		t.Fatal("duplicate polling escalated slow_down before approval redemption")
	case <-time.After(3 * time.Second):
		t.Fatal("approved code was not redeemed at the announced interval")
	}
	waitFor(t, "automatic registration after one compliant poll", func() bool {
		s, e := f.broker.session("agent-one")
		if e != nil || s.RegistrationID != original.RegistrationID || s.Generation <= original.Generation {
			return false
		}
		current.mu.Lock()
		defer current.mu.Unlock()
		return current.started && !current.failed
	})
	p.mu.Lock()
	polls, slow := p.polls, p.slow
	p.mu.Unlock()
	if polls != 2 || slow != 0 {
		t.Fatalf("replacement exchange polling: calls=%d slow_down=%d", polls, slow)
	}
}
