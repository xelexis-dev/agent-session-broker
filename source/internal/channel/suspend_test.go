package channel

import (
	"context"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentResponseCannotEraseSuspension(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old-request" {
			close(started)
			<-release
			w.WriteHeader(204)
			return
		}
		http.Error(w, "session suspended", http.StatusLocked)
	}))
	defer server.Close()
	var suspended atomic.Bool
	client := &http.Client{Transport: headerTransport{next: http.DefaultTransport, suspended: &suspended}}
	done := make(chan error, 1)
	go func() {
		r, err := client.Get(server.URL + "/old-request")
		if err == nil {
			r.Body.Close()
		}
		done <- err
	}()
	<-started
	r, err := client.Get(server.URL + "/suspended")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !suspended.Load() {
		t.Fatal("late success erased the suspension response")
	}
}

func TestInitialBrokerToolsDoesNotRetryInvalidToken(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	u := newTestBrokerUpstream(t.Context(), server.URL)
	defer u.Close()
	backoffs := 0
	_, err := initialBrokerTools(t.Context(), u, func(int) time.Duration { backoffs++; return 0 })
	if err == nil || backoffs != 0 {
		t.Fatal("invalid credentials retried as suspended")
	}
}

type suspendRecoveryUpstream struct{ lookups, resumes int }

func (u *suspendRecoveryUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name == "get_session" {
		u.lookups++
		if u.lookups == 1 {
			return nil, errors.New("suspended during revision reconciliation")
		}
		return &mcp.CallToolResult{StructuredContent: broker.Session{Revision: 5}}, nil
	}
	u.resumes++
	if u.lookups < 2 {
		return &mcp.CallToolResult{StructuredContent: broker.RegisterResult{Code: broker.CodeRevisionMismatch}}, nil
	}
	return &mcp.CallToolResult{StructuredContent: broker.RegisterResult{Session: broker.Session{Name: "retry", Revision: 6, Generation: 2}}}, nil
}

func TestSuspendDuringRevisionLookupRemainsRecoverable(t *testing.T) {
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: t.TempDir(), Name: "retry"}
	opts := durableOptions{state: state, project: "test", scope: "test", timeout: time.Second, backoff: func(int) time.Duration { return time.Millisecond }}
	d, unlock, err := opts.loadRegistration(id)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	d.registrationID = "AAAAAAAAAAAAAAAAAAAAAAAAAA"
	d.revision = 1
	b := &ingressBinding{mu: &sync.Mutex{}, identity: id, durable: d}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	u := &suspendRecoveryUpstream{}
	s, ok := resumeDurable(ctx, u, b, opts, "")
	if !ok || s.Revision != 6 || u.lookups != 2 {
		t.Fatalf("recovery stopped on transient lookup failure: ok=%v revision=%d lookups=%d", ok, s.Revision, u.lookups)
	}
}

func TestInitialBrokerToolsWaitsForUnsuspend(t *testing.T) {
	central := recoveryBroker(t)
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			http.Error(w, "session suspended", http.StatusLocked)
			return
		}
		central.Config.Handler.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	upstream := newTestBrokerUpstream(ctx, proxy.URL+"/mcp?project=brk")
	defer upstream.Close()
	tools, err := initialBrokerTools(ctx, upstream, func(int) time.Duration { return time.Millisecond })
	if err != nil || len(tools) == 0 || calls.Load() < 3 {
		t.Fatalf("did not wait for unsuspend: tools=%d calls=%d err=%v", len(tools), calls.Load(), err)
	}
}

func TestSuspendedBootstrapStopsOnCancellation(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "session suspended", http.StatusLocked) }))
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	upstream := newTestBrokerUpstream(ctx, proxy.URL)
	defer upstream.Close()
	if _, err := initialBrokerTools(ctx, upstream, func(int) time.Duration { return time.Second }); err == nil || ctx.Err() == nil {
		t.Fatal("suspended startup ignored cancellation")
	}
}

func TestDurableClientResumesAfterSuspendAndRestart(t *testing.T) {
	upstream := newSwitchableUpstream(t)
	upstream.service.SetAdmission(grantAdmission{name: "automatic"})
	state := t.TempDir()
	first := newDurableFixtureWithPolicy(t, upstream, state, true)
	first.attach(t, "automatic")
	waitFor(t, "automatic registration", func() bool { s, e := upstream.session("automatic"); return e == nil && s.AutomaticDeliveryReady })
	if err := upstream.service.SetSuspended(context.Background(), "name:automatic", true); err != nil {
		t.Fatal(err)
	}
	first.stop()
	upstream.restart()
	upstream.service.SetAdmission(grantAdmission{name: "automatic"})
	attempted := make(chan struct{}, 1)
	next := newDurableFixtureWithPolicy(t, upstream, state, true, func(opts *durableOptions, _ *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) {
		opts.resumeClock = devicePollClock{sleep: func(ctx context.Context, delay time.Duration) error {
			select {
			case attempted <- struct{}{}:
			default:
			}
			return realPollClock().sleep(ctx, 10*time.Millisecond)
		}}
	})
	select {
	case <-attempted:
	case <-time.After(time.Second):
		t.Fatal("suspended resume terminated instead of waiting")
	}
	s, e := upstream.session("automatic")
	if e != nil || s.AutomaticDeliveryReady || s.Status != broker.StatusSuspended {
		t.Fatalf("suspend lost: %+v %v", s, e)
	}
	if err := upstream.service.SetSuspended(context.Background(), "name:automatic", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "automatic unsuspend", func() bool { s, e := upstream.session("automatic"); return e == nil && s.AutomaticDeliveryReady })
	if err := call(context.Background(), upstream, "send_message", broker.Message{From: "automatic", To: "automatic", Body: "new after unsuspend"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-next.sink:
		if message.Body != "new after unsuspend" {
			t.Fatal("stale message")
		}
	case <-time.After(time.Second):
		t.Fatal("no delivery after unsuspend")
	}
}
