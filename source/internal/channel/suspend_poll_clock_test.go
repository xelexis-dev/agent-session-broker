package channel

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type clockedResumeUpstream struct {
	*brokerUpstream
	respond func() (broker.RegisterResult, error)
}

// This clock-only fake has no transport or server capability handshake.
func (u *clockedResumeUpstream) verifyMessageAck(context.Context) bool { return false }

func (u *clockedResumeUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name != "resume_session" {
		return nil, errors.New("unexpected tool")
	}
	result, err := u.respond()
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{StructuredContent: result}, nil
}
func suspendClockBinding(t *testing.T) (*ingressBinding, durableOptions) {
	t.Helper()
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: t.TempDir(), Name: "suspend-clock"}
	opts := durableOptions{backoff: resumeFixtureBackoff, state: state, scope: "suspend-clock", project: "test", timeout: time.Second}.withDefaults()
	d, unlock, err := opts.loadRegistration(id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	d.registrationID = "AAAAAAAAAAAAAAAAAAAAAAAAAA"
	d.revision = 1
	return &ingressBinding{mu: &sync.Mutex{}, identity: id, durable: d}, opts
}
func resumeClockSuccess() broker.RegisterResult {
	return broker.RegisterResult{Session: broker.Session{Name: "suspend-clock", Status: broker.StatusAlive, Revision: 2, Generation: 2}}
}

func TestSuspendedResumeFakeClockAliveWithinTwoSeconds(t *testing.T) {
	for _, http423 := range []bool{false, true} {
		t.Run(map[bool]string{false: "protocol", true: "http423-wrapper"}[http423], func(t *testing.T) {
			b, opts := suspendClockBinding(t)
			var elapsed time.Duration
			var at []time.Duration
			opts.resumeClock = devicePollClock{sleep: func(ctx context.Context, d time.Duration) error { elapsed += d; return ctx.Err() }}
			u := &clockedResumeUpstream{brokerUpstream: &brokerUpstream{}}
			// Unsuspend between polls after the exponential baseline would have grown
			// beyond 2 seconds. The next permitted resume is at virtual second eight.
			const unsuspendAt = 7 * time.Second
			u.respond = func() (broker.RegisterResult, error) {
				at = append(at, elapsed)
				if elapsed >= unsuspendAt {
					return resumeClockSuccess(), nil
				}
				if http423 {
					u.suspended.Store(true)
					return broker.RegisterResult{}, errors.New("HTTP 423")
				}
				return broker.RegisterResult{Code: broker.CodeSuspended}, nil
			}
			session, ok := resumeDurable(t.Context(), opts.wrapReceive(u, ""), b, opts, "")
			if !ok || session.Status != broker.StatusAlive || elapsed-unsuspendAt > 2*time.Second {
				t.Fatalf("alive=%v elapsed=%s", ok, elapsed)
			}
			if !reflect.DeepEqual(at, []time.Duration{0, 2 * time.Second, 4 * time.Second, 6 * time.Second, 8 * time.Second}) {
				t.Fatalf("resume times=%v", at)
			}
			if u.suspended.Load() {
				t.Fatal("successful resume retained HTTP suspension latch")
			}
		})
	}
}

func TestResumeOtherFailuresKeepExponentialSchedule(t *testing.T) {
	for _, network := range []bool{false, true} {
		t.Run(map[bool]string{false: "storage", true: "network"}[network], func(t *testing.T) {
			b, opts := suspendClockBinding(t)
			var elapsed time.Duration
			var at []time.Duration
			opts.resumeClock = devicePollClock{sleep: func(ctx context.Context, d time.Duration) error { elapsed += d; return ctx.Err() }}
			u := &clockedResumeUpstream{brokerUpstream: &brokerUpstream{}}
			u.respond = func() (broker.RegisterResult, error) {
				at = append(at, elapsed)
				if len(at) == 7 {
					return resumeClockSuccess(), nil
				}
				if network {
					return broker.RegisterResult{}, errors.New("network unavailable")
				}
				return broker.RegisterResult{Code: broker.CodeStorage}, nil
			}
			if _, ok := resumeDurable(t.Context(), u, b, opts, ""); !ok {
				t.Fatal("did not resume")
			}
			if !reflect.DeepEqual(at, []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second, 15 * time.Second, 25 * time.Second, 35 * time.Second}) {
				t.Fatalf("other failure schedule changed: %v", at)
			}
		})
	}
}

func TestSuspendedResumeStopsOnTerminalCodeAndContext(t *testing.T) {
	for _, code := range []string{broker.CodeDetached, broker.CodeNotFound, "cancel"} {
		t.Run(code, func(t *testing.T) {
			b, opts := suspendClockBinding(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls, waits := 0, 0
			opts.resumeClock = devicePollClock{sleep: func(ctx context.Context, d time.Duration) error {
				waits++
				if d != 2*time.Second {
					t.Errorf("wait=%s want2s", d)
				}
				if code == "cancel" {
					cancel()
				}
				return ctx.Err()
			}}
			u := &clockedResumeUpstream{brokerUpstream: &brokerUpstream{}, respond: func() (broker.RegisterResult, error) {
				calls++
				if calls == 1 {
					return broker.RegisterResult{Code: broker.CodeSuspended}, nil
				}
				return broker.RegisterResult{Code: code}, nil
			}}
			if _, ok := resumeDurable(ctx, u, b, opts, ""); ok {
				t.Fatal("unexpected success")
			}
			want := 2
			if code == "cancel" {
				want = 1
			}
			if calls != want || waits != 1 {
				t.Fatalf("calls=%d waits=%d", calls, waits)
			}
		})
	}
}

func TestRenewSuspensionAndDefinitiveOtherResumeCode(t *testing.T) {
	b, opts := suspendClockBinding(t)
	opts.diagnoseRenew(b, nil, broker.RegisterResult{Code: broker.CodeSuspended}, nil)
	var waits []time.Duration
	opts.resumeClock = devicePollClock{sleep: func(ctx context.Context, d time.Duration) error { waits = append(waits, d); return ctx.Err() }}
	calls := 0
	u := &clockedResumeUpstream{brokerUpstream: &brokerUpstream{}, respond: func() (broker.RegisterResult, error) {
		calls++
		if calls == 1 {
			return broker.RegisterResult{}, errors.New("lost response after suspended renew")
		}
		if calls <= 3 {
			return broker.RegisterResult{Code: broker.CodeStorage}, nil
		}
		return resumeClockSuccess(), nil
	}}
	if _, ok := resumeDurable(t.Context(), u, b, opts, ""); !ok {
		t.Fatal("did not resume")
	}
	if !reflect.DeepEqual(waits, []time.Duration{2 * time.Second, 2 * time.Second, 4 * time.Second}) {
		t.Fatalf("renew suspension/other code waits=%v", waits)
	}
}

func TestDefinitiveResumeCodeClearsStaleHTTPLatch(t *testing.T) {
	b, opts := suspendClockBinding(t)
	var waits []time.Duration
	opts.resumeClock = devicePollClock{sleep: func(ctx context.Context, d time.Duration) error { waits = append(waits, d); return ctx.Err() }}
	calls := 0
	u := &clockedResumeUpstream{brokerUpstream: &brokerUpstream{}}
	u.respond = func() (broker.RegisterResult, error) {
		calls++
		switch calls {
		case 1:
			u.suspended.Store(true)
			return broker.RegisterResult{}, errors.New("HTTP 423")
		case 2, 3:
			return broker.RegisterResult{Code: broker.CodeStorage}, nil
		case 4:
			return broker.RegisterResult{}, errors.New("network unavailable")
		default:
			return resumeClockSuccess(), nil
		}
	}
	if _, ok := resumeDurable(t.Context(), opts.wrapReceive(u, ""), b, opts, ""); !ok {
		t.Fatal("did not resume")
	}
	if !reflect.DeepEqual(waits, []time.Duration{2 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}) {
		t.Fatalf("stale 423 overrode later code: %v", waits)
	}
}
