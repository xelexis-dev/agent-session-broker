package channel

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

type interruptCalls struct {
	mu        sync.Mutex
	channelID string
	reports   []map[string]string
}

func (c *interruptCalls) snapshot() (string, []map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.channelID, append([]map[string]string(nil), c.reports...)
}

type interruptUpstream struct {
	inner        Upstream
	calls        *interruptCalls
	rejectReport bool
	beforeRenew  func(context.Context)
}

func (u *interruptUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	switch p.Name {
	case "renew_lease":
		if u.beforeRenew != nil {
			u.beforeRenew(ctx)
		}
	case "register_session", "attach_session":
		raw, err := json.Marshal(p.Arguments)
		if err != nil {
			return nil, err
		}
		var lease struct{ ChannelID string }
		if err := json.Unmarshal(raw, &lease); err != nil {
			return nil, err
		}
		u.calls.mu.Lock()
		u.calls.channelID = lease.ChannelID
		u.calls.mu.Unlock()
	case "report_ingress_failure":
		raw, err := json.Marshal(p.Arguments)
		if err != nil {
			return nil, err
		}
		var report map[string]string
		if err := json.Unmarshal(raw, &report); err != nil {
			return nil, err
		}
		u.calls.mu.Lock()
		u.calls.reports = append(u.calls.reports, report)
		u.calls.mu.Unlock()
		if u.rejectReport {
			return u.inner.CallTool(ctx, p)
		}
		// Observe the hook's report without retiring the broker lease, which
		// would independently make the background receiver report its exit.
		return &mcp.CallToolResult{}, nil
	}
	return u.inner.CallTool(ctx, p)
}

func TestInterruptReportsOnlyBoundUpstream(t *testing.T) {
	const sessionA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const sessionB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	for _, failPush := range []bool{true, false} {
		name := "success"
		if failPush {
			name = "push_failure"
		}
		t.Run(name, func(t *testing.T) {
			auth := newDeviceAuthServer(t)
			brokerUpstream := newSwitchableUpstream(t)
			callsA, callsB, sharedCalls := &interruptCalls{}, &interruptCalls{}, &interruptCalls{}
			var pushes atomic.Int32
			var armed atomic.Bool
			renewEntered, releaseRenew := make(chan struct{}), make(chan struct{})

			var renewOnce, releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseRenew) }) }
			t.Cleanup(release)
			routerA := newCodexIngress(func(context.Context, broker.Message) error {
				pushes.Add(1)
				if failPush {
					return errors.New("test interrupt push failed")
				}
				return nil
			})
			routerB := newCodexIngress(func(context.Context, broker.Message) error { return nil })
			identities := map[string]broker.AttachRequest{
				sessionA: {Harness: "codex", SessionID: sessionA, PID: 7, CWD: t.TempDir()},
				sessionB: {Harness: "codex", SessionID: sessionB, PID: 7, CWD: t.TempDir()},
			}
			f := newDeviceModeFixture(t, auth, brokerUpstream, t.TempDir(), identities, func(f *deviceModeFixture) {
				f.configureDurable = func(o *durableOptions) { o.timeout = 30 * time.Second }
				f.sharedUpstream = &interruptUpstream{inner: unavailableUpstream{}, calls: sharedCalls, rejectReport: true}
				f.wrapUpstream = func(credential string, inner Upstream) Upstream {
					calls := callsA
					if credential == "asb_s_b" {
						calls = callsB
					}
					up := &interruptUpstream{inner: inner, calls: calls}
					if calls == callsA {
						up.beforeRenew = func(ctx context.Context) {
							if armed.Load() {
								renewOnce.Do(func() { close(renewEntered) })
								select {
								case <-releaseRenew:
								case <-ctx.Done():
								}
							}
						}
					}
					return up
				}
				f.sinkFactory = func(id broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
					router := routerA
					if id.SessionID == sessionB {
						router = routerB
					}
					return router.deliver, router, nil
				}
			})
			for _, session := range []string{sessionA, sessionB} {
				if status := f.status(session); status["status"] != "pending" {
					t.Fatalf("authorization was not pending: %v", status)
				}
			}
			f.grant(sessionA, "asb_s_a", "agent-a")
			f.grant(sessionB, "asb_s_b", "agent-b")
			for _, name := range []string{"agent-a", "agent-b"} {
				waitFor(t, name+" registered", func() bool {
					s, err := brokerUpstream.session(name)
					return err == nil && s.AutomaticDeliveryReady
				})
			}
			registered, err := brokerUpstream.session("agent-a")
			if err != nil || registered.RegistrationID == "" {
				t.Fatalf("missing durable registration: %v", err)
			}
			writeCodexHook(routerA, "UserPromptSubmit")
			if err := routerA.deliver(context.Background(), broker.Message{From: "agent-b", To: "agent-a", Body: "pending"}); err != nil {
				t.Fatal(err)
			}
			// This fixture swallows lease retirement. Park its next renewal so
			// receiver retries/exit reports cannot contaminate hook-only counts.
			armed.Store(true)
			select {
			case <-renewEntered:
			case <-time.After(5 * time.Second):
				t.Fatal("renewal did not reach barrier")
			}
			defer release()
			result := f.call(sessionA, "ingress_hook", map[string]string{"session_id": sessionA, "hook_event_name": "Interrupt"})
			if result.IsError != failPush || pushes.Load() != 1 {
				t.Fatalf("interrupt: error=%v pushes=%d", result.IsError, pushes.Load())
			}
			if failPush {
				raw, _ := json.Marshal(result)
				if !strings.Contains(string(raw), "test interrupt push failed") {
					t.Fatalf("push error was replaced: %s", raw)
				}
			}
			channelID, reportsA := callsA.snapshot()
			_, reportsB := callsB.snapshot()
			_, sharedReports := sharedCalls.snapshot()
			wantA := 0
			if failPush {
				wantA = 1
			}
			if len(reportsA) != wantA || len(reportsB) != 0 || len(sharedReports) != 0 {
				t.Fatalf("failure report routing: A=%d B=%d shared=%d; want A=%d B=0 shared=0", len(reportsA), len(reportsB), len(sharedReports), wantA)
			}
			t.Logf("failure report routing: A=%d B=%d shared=%d", len(reportsA), len(reportsB), len(sharedReports))
			if failPush {
				want := map[string]string{"sessionId": sessionA, "channelId": channelID, "registrationId": registered.RegistrationID}
				if channelID == "" || !reflect.DeepEqual(reportsA[0], want) {
					t.Fatalf("report identity: got %v want %v", reportsA[0], want)
				}
			}
		})
	}
}

func TestInterruptFailureUsesLegacySharedUpstream(t *testing.T) {
	const session = "11111111-1111-4111-8111-111111111111"
	ctx, cancel := context.WithCancel(context.Background())
	calls := &interruptCalls{}
	up := &interruptUpstream{inner: &ingressUpstream{input: make(chan broker.Message)}, calls: calls}
	router := newCodexIngress(func(context.Context, broker.Message) error { return errors.New("test interrupt push failed") })
	identity := broker.AttachRequest{Harness: "codex", SessionID: session, PID: 123, CWD: t.TempDir()}
	server, shutdown := newIngressServerWithOptions(ctx, up, nil,
		func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) { return identity, nil },
		func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
			return router.deliver, router, nil
		}, durableOptions{})
	t.Cleanup(shutdown)
	t.Cleanup(cancel)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]string{"name": "owner"}})
	if err != nil || result.IsError {
		t.Fatalf("attach: %v %v", result, err)
	}
	writeCodexHook(router, "UserPromptSubmit")
	if err := router.deliver(ctx, broker.Message{From: "peer", To: "owner", Body: "pending"}); err != nil {
		t.Fatal(err)
	}
	result, err = client.CallTool(ctx, &mcp.CallToolParams{Name: "ingress_hook", Meta: mcp.Meta{"threadId": session}, Arguments: map[string]string{"session_id": session, "hook_event_name": "Interrupt"}})
	if err != nil || !result.IsError {
		t.Fatalf("interrupt did not return push error: %v %v", result, err)
	}
	channelID, reports := calls.snapshot()
	want := []map[string]string{{"sessionId": session, "channelId": channelID}}
	if channelID == "" || !reflect.DeepEqual(reports, want) {
		t.Fatalf("legacy failure report: got %v want %v", reports, want)
	}
}
