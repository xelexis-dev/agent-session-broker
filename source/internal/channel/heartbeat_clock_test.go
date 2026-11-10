package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/localactivity"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type heartbeatUpstream struct {
	mu       sync.Mutex
	attempts []time.Time
	renew    func(context.Context, int) (broker.RegisterResult, error)
}

func (u *heartbeatUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name != "renew_lease" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	u.mu.Lock()
	u.attempts = append(u.attempts, time.Now())
	n := len(u.attempts)
	u.mu.Unlock()
	out := broker.RegisterResult{Session: broker.Session{Generation: 1}, MessageAckVersion: 1, ActivityVersion: broker.ActivityVersion, RuntimeInfoVersion: broker.RuntimeInfoVersion}
	var err error
	if u.renew != nil {
		out, err = u.renew(ctx, n)
	}
	return &mcp.CallToolResult{StructuredContent: out}, err
}
func (u *heartbeatUpstream) times() []time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]time.Time(nil), u.attempts...)
}
func (*heartbeatUpstream) activityAllowed() bool               { return true }
func (*heartbeatUpstream) verifyActivity(context.Context) bool { return true }
func (*heartbeatUpstream) runtimeInfoAllowed() bool            { return true }
func heartbeatBinding() *ingressBinding {
	return &ingressBinding{mu: &sync.Mutex{}, identity: broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", ChannelID: "lease"}, durable: &durableRegistration{clientID: "client", registrationID: "registration"}, generation: 1}
}
func startHeartbeat(t *testing.T, u Upstream, b *ingressBinding, opts durableOptions) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	opts = opts.withDefaults()
	go func() {
		defer close(done)
		receiveDurableOnce(ctx, u, b, opts, func(context.Context, broker.Message) error { return nil })
	}()
	synctest.Wait()
	return func() { cancel(); <-done }
}
func TestHeartbeatHealthyMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		u := &heartbeatUpstream{}
		stop := startHeartbeat(t, u, heartbeatBinding(), durableOptions{})
		defer stop()
		time.Sleep(time.Minute)
		synctest.Wait()
		times := u.times()
		if len(times) != 6 {
			t.Fatalf("healthy renew count=%d, want6", len(times))
		}
		for i := 1; i < len(times); i++ {
			if times[i].Sub(times[i-1]) != 10*time.Second {
				t.Fatal("healthy cadence not10s")
			}
		}
	})
}
func TestActivityMaxAgeHeartbeatPlusFive(t *testing.T) {
	for _, age := range []time.Duration{12 * time.Second, 15 * time.Second, 15*time.Second + time.Nanosecond} {
		t.Run(age.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := heartbeatBinding()
				b.activityEnabled = true
				b.runtimeInfoEnabled = true
				s := (durableOptions{heartbeat: 10 * time.Second}).newActivityStream(b)
				s.pending = make(chan localactivity.Observation, 1)
				s.pending <- localactivity.Observation{State: localactivity.Working, Source: "codex-rollout", ObservedAt: time.Now().UTC(), Model: "fixture-model", Effort: "high"}
				time.Sleep(age)
				var a broker.Activity
				json.Unmarshal(s.sample(t.Context(), b, &heartbeatUpstream{}), &a)
				var r broker.RuntimeInfo
				json.Unmarshal(s.runtimeInfo, &r)
				if age <= 15*time.Second {
					if a.State != broker.ActivityWorking || r.Model != "fixture-model" {
						t.Fatal("sample within hb+5 discarded")
					}
				} else if a.State != broker.ActivityUnknown || r.Model != "" {
					t.Fatal("expired sample retained")
				}
			})
		})
	}
}

func TestHeartbeatRetryFromAttemptStart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures int
		slow     time.Duration
	}{{"one-timeout", 1, 0}, {"two-timeouts", 2, 0}, {"two-timeouts-slow-success", 2, 4 * time.Second}, {"slow-success", 0, 4 * time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				deadline := start.Add(30 * time.Second)
				u := &heartbeatUpstream{}
				u.renew = func(ctx context.Context, n int) (broker.RegisterResult, error) {
					if n <= tc.failures {
						<-ctx.Done()
						return broker.RegisterResult{}, ctx.Err()
					}
					time.Sleep(tc.slow)
					if !time.Now().Before(deadline) {
						t.Error("renew reached expired30s lease")
					}
					deadline = time.Now().Add(30 * time.Second)
					return broker.RegisterResult{Session: broker.Session{Generation: 1}}, nil
				}
				stop := startHeartbeat(t, u, heartbeatBinding(), durableOptions{})
				defer stop()
				time.Sleep(time.Minute)
				synctest.Wait()
				times := u.times()
				if len(times) < tc.failures+1 {
					t.Fatal("missing timeout retry")
				}
				for i := 0; i <= tc.failures; i++ {
					if times[i].Sub(start) != 10*time.Second+time.Duration(i)*5*time.Second {
						t.Fatal("retry not aligned to attempt start")
					}
				}
				for i := tc.failures + 1; i < len(times); i++ {
					if times[i].Sub(times[i-1]) != 10*time.Second {
						t.Fatal("success did not restore10s cadence")
					}
				}
			})
		})
	}
}

func TestHeartbeatErrorExitClassification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout bool
		slow    time.Duration
	}{{"ordinary", false, 0}, {"slow-error", false, 4 * time.Second}, {"three-timeouts", true, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				u := &heartbeatUpstream{}
				u.renew = func(ctx context.Context, _ int) (broker.RegisterResult, error) {
					if tc.timeout {
						<-ctx.Done()
						return broker.RegisterResult{}, ctx.Err()
					}
					time.Sleep(tc.slow)
					return broker.RegisterResult{}, fmt.Errorf("synthetic failure")
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				type ended struct {
					exit receiverExit
					at   time.Time
				}
				done := make(chan ended, 1)
				go func() {
					exit := receiveDurableOnce(ctx, u, heartbeatBinding(), (durableOptions{}).withDefaults(), func(context.Context, broker.Message) error { return nil })
					done <- ended{exit, time.Now()}
				}()
				synctest.Wait()
				time.Sleep(26 * time.Second)
				synctest.Wait()
				select {
				case out := <-done:
					count := 1
					want := 10*time.Second + tc.slow
					if tc.timeout {
						count = 3
						want = 25 * time.Second
					}
					if out.exit != exitUpstream || out.at.Sub(start) != want || len(u.times()) != count {
						t.Fatal("develop error exit behavior changed")
					}
				default:
					t.Fatal("receiver did not exit before lease expiry")
				}
			})
		})
	}
}

func TestHeartbeatTransitionWakesOncePerTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		pool := newActivityWorkers(ctx)
		defer func() { cancel(); pool.wg.Wait() }()
		var observations atomic.Int32
		opts := durableOptions{activityPool: pool, activityFactory: func(localactivity.Config) activityObserver {
			return observerFunc(func(ctx context.Context, _ localactivity.Binding) localactivity.Observation {
				time.Sleep(10 * time.Millisecond)
				state := localactivity.Working
				if observations.Add(1)%2 == 0 {
					state = localactivity.Idle
				}
				return localactivity.Observation{State: state, Source: "codex-rollout", ObservedAt: time.Now().UTC()}
			})
		}}
		u := &heartbeatUpstream{}
		b := heartbeatBinding()
		b.activityEnabled = true
		start := time.Now()
		stop := startHeartbeat(t, u, b, opts)
		defer stop()
		time.Sleep(10100 * time.Millisecond)
		synctest.Wait()
		times := u.times()
		if len(times) != 2 || times[1].Sub(start) >= 20*time.Second {
			t.Fatalf("transition renews=%d, wantone before nexttick", len(times))
		}
		time.Sleep(9 * time.Second)
		synctest.Wait()
		if len(u.times()) != 2 {
			t.Fatal("more thanone transition wake in tick")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if len(u.times()) != 4 {
			t.Fatal("next tick did not re-arm transition wake")
		}
	})
}

func TestHeartbeatUnchangedObservationDoesNotWake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		pool := newActivityWorkers(ctx)
		defer func() { cancel(); pool.wg.Wait() }()
		opts := durableOptions{activityPool: pool, activityFactory: func(localactivity.Config) activityObserver {
			return observerFunc(func(context.Context, localactivity.Binding) localactivity.Observation {
				return localactivity.Observation{State: localactivity.Idle, Source: "codex-rollout", ObservedAt: time.Now().UTC()}
			})
		}}
		u := &heartbeatUpstream{}
		b := heartbeatBinding()
		b.activityEnabled = true
		stop := startHeartbeat(t, u, b, opts)
		defer stop()
		time.Sleep(61 * time.Second)
		synctest.Wait()
		if len(u.times()) != 7 {
			t.Fatalf("renew count=%d, want 6 periodic plus initial unknown-to-idle transition", len(u.times()))
		}
	})
}

func TestHeartbeatOldTTLDisplayGap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var mu sync.Mutex
		lastSuccess := start
		leaseUntil := start.Add(30 * time.Second)
		u := &heartbeatUpstream{}
		u.renew = func(ctx context.Context, n int) (broker.RegisterResult, error) {
			if n == 2 {
				<-ctx.Done()
				return broker.RegisterResult{}, ctx.Err()
			}
			if n == 3 {
				time.Sleep(4 * time.Second)
			}
			mu.Lock()
			lastSuccess = time.Now()
			leaseUntil = time.Now().Add(30 * time.Second)
			mu.Unlock()
			return broker.RegisterResult{Session: broker.Session{Generation: 1}}, nil
		}
		stop := startHeartbeat(t, u, heartbeatBinding(), durableOptions{})
		defer stop()
		time.Sleep(26 * time.Second)
		synctest.Wait()
		mu.Lock()
		last, until := lastSuccess, leaseUntil
		mu.Unlock()
		if time.Now().Before(last.Add(15 * time.Second)) {
			t.Fatal("old TTL did not expire")
		}
		if !time.Now().Before(last.Add(25*time.Second)) || !time.Now().Before(until) {
			t.Fatal("new TTL or lease expired")
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		mu.Lock()
		last = lastSuccess
		mu.Unlock()
		if last.Sub(start) != 29*time.Second {
			t.Fatal("slow retry not recovered at29s")
		}
		t.Log("old15s TTL display gap25s..29s (4s); new25s TTL and30s lease remain live")
	})
}

func TestHeartbeatControlWakesRemainImmediate(t *testing.T) {
	for _, kind := range []string{"ack", "receive"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				opts := (durableOptions{}).withDefaults()
				u := &heartbeatUpstream{}
				b := heartbeatBinding()
				var up Upstream = u
				var wake chan struct{}
				if kind == "ack" {
					wake = make(chan struct{}, 1)
					b.messageAck = &messageAckState{wake: wake}
				} else {
					up = opts.wrapReceive(u, "synthetic-credential")
					wake = up.(*receiveUpstream).currentPermission().wake
				}
				start := time.Now()
				stop := startHeartbeat(t, up, b, opts)
				defer stop()
				time.Sleep(time.Second)
				wake <- struct{}{}
				synctest.Wait()
				times := u.times()
				if len(times) != 1 || times[0].Sub(start) != time.Second {
					t.Fatal("control wake waited for10s tick")
				}
			})
		})
	}
}
