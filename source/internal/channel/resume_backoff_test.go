package channel

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type refusalUpstream struct {
	resumes int
	cancel  context.CancelFunc
}

func (u *refusalUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	switch p.Name {
	case "wait_for_message":
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "remote ingress lease is no longer active"}}}, nil
	case "resume_session":
		u.resumes++
		if u.resumes == 100 {
			u.cancel()
		}
		return &mcp.CallToolResult{StructuredContent: broker.RegisterResult{Session: broker.Session{Name: "test", RegistrationID: "reg", Revision: uint64(u.resumes + 1)}}}, nil
	default:
		return &mcp.CallToolResult{StructuredContent: map[string]any{}}, nil
	}
}
func TestResumeRefusalBackoffTenSeconds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	up := &refusalUpstream{cancel: cancel}
	b := &ingressBinding{mu: &sync.Mutex{}, identity: broker.AttachRequest{Name: "test", ChannelID: "lease"}, durable: &durableRegistration{registrationID: "reg", key: "key"}}
	var elapsed time.Duration
	opts := durableOptions{state: store, heartbeat: time.Hour, timeout: time.Second, backoff: func(n int) time.Duration { return time.Second << min(n, 3) }, resumeClock: devicePollClock{now: time.Now, sleep: func(ctx context.Context, d time.Duration) error {
		elapsed += d
		if elapsed > 10*time.Second {
			cancel()
			return context.Canceled
		}
		return nil
	}}}
	runDurableReceiver(ctx, up, b, opts, func(context.Context, broker.Message) error { return nil })
	if up.resumes > 3 {
		t.Fatalf("resumes per 10s = %d, want <=3 (elapsed %s)", up.resumes, elapsed)
	}
	if up.resumes != 3 {
		t.Fatalf("resumes=%d want 3", up.resumes)
	}
}

func TestResumeBackoffProgressReset(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		t.Run(fmt.Sprint(delivered), func(t *testing.T) {
			store, err := clientstate.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			b := heartbeatBinding()
			b.durable.key = "key"
			run, wait := 0, 0
			up := round2CallFunc(func(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
				switch p.Name {
				case "resume_session":
					run++
					wait = 0
					return &mcp.CallToolResult{StructuredContent: broker.RegisterResult{Session: broker.Session{Name: "test", RegistrationID: "registration"}}}, nil
				case "wait_for_message":
					wait++
					if delivered && run == 2 && wait == 1 {
						return &mcp.CallToolResult{StructuredContent: broker.Message{RegistrationID: "registration"}}, nil
					}
					return &mcp.CallToolResult{IsError: true}, nil
				default:
					return &mcp.CallToolResult{StructuredContent: map[string]any{}}, nil
				}
			})
			var attempts []int
			opts := durableOptions{state: store, heartbeat: time.Hour, backoff: func(n int) time.Duration { attempts = append(attempts, n); return time.Second }, resumeClock: devicePollClock{now: time.Now, sleep: func(context.Context, time.Duration) error {
				if len(attempts) == 4 {
					cancel()
					return context.Canceled
				}
				return nil
			}}}
			runDurableReceiver(ctx, up, b, opts, func(context.Context, broker.Message) error { return nil })
			want := []int{0, 1, 2, 3}
			if delivered {
				want = []int{0, 1, 0, 1}
			}
			if !reflect.DeepEqual(attempts, want) {
				t.Fatalf("attempts=%v want=%v", attempts, want)
			}
		})
	}
}

func TestResumeBackoffSleepDoesNotHoldBindingLocks(t *testing.T) {
	for _, retire := range []bool{false, true} {
		t.Run(fmt.Sprint(retire), func(t *testing.T) {
			store, err := clientstate.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			b := heartbeatBinding()
			b.durable.key = "key"
			up := &refusalUpstream{cancel: cancel}
			opts := durableOptions{state: store, heartbeat: time.Hour, backoff: func(int) time.Duration { return time.Hour }, resumeClock: devicePollClock{now: time.Now, sleep: func(ctx context.Context, _ time.Duration) error {
				// Attach/rename serialize on registerMu; retire uses mu.
				if !b.registerMu.TryLock() {
					t.Error("registerMu held during backoff")
				} else {
					b.registerMu.Unlock()
				}
				if !b.mu.TryLock() {
					t.Error("binding mu held during backoff")
				} else {
					b.retired = retire
					b.mu.Unlock()
				}
				if retire {
					return nil
				}
				cancel()
				return ctx.Err()
			}}}
			runDurableReceiver(ctx, up, b, opts, func(context.Context, broker.Message) error { return nil })
			if up.resumes != 0 {
				t.Fatal("resumed after cancellation/retirement")
			}
		})
	}
}

func TestResumeProgressHeartbeatValidation(t *testing.T) {
	for _, kind := range []string{"heartbeat", "early-wake", "activation", "bad-ack", "suspended"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var progress atomic.Bool
				b := heartbeatBinding()
				opts := (durableOptions{runProgress: &progress}).withDefaults()
				up := &heartbeatUpstream{}
				if kind == "bad-ack" {
					b.messageAck = &messageAckState{wake: make(chan struct{}, 1)}
					up.renew = func(context.Context, int) (broker.RegisterResult, error) { return broker.RegisterResult{}, nil }
				}
				if kind == "suspended" {
					up.renew = func(context.Context, int) (broker.RegisterResult, error) {
						return broker.RegisterResult{Code: broker.CodeSuspended}, nil
					}
				}
				if kind == "early-wake" {
					b.messageAck = &messageAckState{wake: make(chan struct{}, 1)}
				}
				if kind == "activation" {
					ready := make(chan struct{})
					close(ready)
					opts.ready = ready
				}
				stop := startHeartbeat(t, up, b, opts)
				defer stop()
				if kind == "early-wake" {
					for range 11 {
						time.Sleep(time.Second)
						b.messageAck.signal()
						synctest.Wait()
					}
				}
				if kind == "heartbeat" || kind == "bad-ack" || kind == "suspended" {
					time.Sleep(10 * time.Second)
					synctest.Wait()
				}
				if progress.Load() != (kind == "heartbeat") {
					t.Fatalf("progress=%v", progress.Load())
				}
			})
		})
	}
}

func TestResumeProgressDurableCompletion(t *testing.T) {
	for _, kind := range []string{"done", "queued", "replay", "failed"} {
		t.Run(kind, func(t *testing.T) {
			store, err := clientstate.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			b := heartbeatBinding()
			ack := &messageAckState{store: store, scope: "scope", registration: "registration", epoch: "epoch", lease: b.identity.ChannelID, completed: map[uint64]bool{}, wake: make(chan struct{}, 1), failed: make(chan struct{})}
			b.messageAck = ack
			if kind == "replay" {
				if err := store.SaveDeliveryWatermark("scope", "registration", "epoch", 1); err != nil {
					t.Fatal(err)
				}
			}
			// A deferred native queue must signal completion explicitly, not via sink(nil).
			b.router = &codexIngress{}
			var progress atomic.Bool
			n := 0
			up := round2CallFunc(func(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
				if p.Name == "wait_for_message" {
					n++
					if n == 1 {
						return &mcp.CallToolResult{StructuredContent: broker.Message{RegistrationID: "registration", DeliverySequence: 1}}, nil
					}
					return &mcp.CallToolResult{IsError: true}, nil
				}
				return &mcp.CallToolResult{StructuredContent: broker.RegisterResult{MessageAckVersion: 1}}, nil
			})
			var saved handoffCompletion
			exit := receiveDurableOnce(ctx, up, b, (durableOptions{runProgress: &progress, heartbeat: time.Hour}).withDefaults(), func(ctx context.Context, _ broker.Message) error {
				saved = handoff(ctx)
				if kind == "done" {
					return saved.done()
				}
				if kind == "failed" {
					return errors.New("sink failed")
				}
				return nil
			})
			if progress.Load() != (kind == "done") {
				t.Fatalf("exit=%v progress=%v", exit, progress.Load())
			}
			if kind == "queued" {
				if err := saved.done(); err != nil {
					t.Fatal(err)
				}
				if progress.Load() {
					t.Fatal("late completion counted in ended run")
				}
			}
		})
	}
}

func TestResumeDefaultJitterBounds(t *testing.T) {
	backoff := (durableOptions{}).withDefaults().backoff
	for attempt, base := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second} {
		for range 100 {
			d := backoff(attempt)
			if d < base*8/10 || d > base*12/10 {
				t.Fatalf("attempt=%d delay=%s", attempt, d)
			}
		}
	}
	injected := func(int) time.Duration { return 17 * time.Second }
	if got := (durableOptions{backoff: injected}).withDefaults().backoff(0); got != 17*time.Second {
		t.Fatal("injected backoff jittered")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := realPollClock().sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal("sleep ignored cancellation")
	}
}

func TestWaitRefusalDiagnosticsSafe(t *testing.T) {
	store, openErr := clientstate.Open(t.TempDir())
	if openErr != nil {
		t.Fatal(openErr)
	}
	key := clientstate.RegistrationKey("scope", "claude", "11111111-1111-4111-8111-111111111111")

	for message, want := range map[string]string{"remote ingress lease is no longer active": "lease-replaced", "remote delivery channel is not activated": "not-activated", "invalid message acknowledgement or cursor": "ack-invalid", "message acknowledgement requires the current registration and epoch": "ack-invalid", "private payload remote ingress lease is no longer active": "other"} {
		up := round2CallFunc(func(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: message}}}, nil
		})
		err := call(t.Context(), up, "wait_for_message", nil, nil)
		if got := diagnosticErrorClass(err, false); got != want {
			t.Fatalf("class=%s want=%s", got, want)
		}
		summary := clientstate.SafeDiagnostics(clientstate.Diagnostics{Summary: clientstate.DiagnosticSummary{LastFailureClass: want, LastFailureCode: want}}).Summary
		if summary.LastFailureClass != want || summary.LastFailureCode != want {
			t.Fatal("diagnostic class hidden by safe projection")
		}
		event := clientstate.DiagnosticEvent{At: time.Now().UTC(), SessionID: "11111111-1111-4111-8111-111111111111", Event: "receive-failed", Reason: want, ErrorClass: want}
		if err := store.RecordDiagnostic(key, event); err != nil {
			t.Fatal(err)
		}
		saved, loadErr := store.LoadDiagnostics(key)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		safe := clientstate.SafeDiagnostics(saved)
		if safe.Summary.LastFailureClass != want || safe.Summary.LastFailureCode != want {
			t.Fatal("stored diagnosis lost class")
		}
		if strings.Contains(err.Error(), "private payload") {
			t.Fatal("payload leaked")
		}
	}
}

// Exact schedule fixtures inject the pre-jitter schedule; production defaults jitter.
func resumeFixtureBackoff(attempt int) time.Duration {
	return min(time.Second<<uint(min(attempt, 4)), 10*time.Second)
}
