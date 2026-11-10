package channel

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"sync"
	"testing"
	"time"
)

type heartbeatScopeFixture struct {
	Upstream
	mu                        sync.Mutex
	renews, receives, resumes int
	widenAt                   int
	active, maxActive         int
	version                   uint64
	scopes                    []string
	session                   broker.Session
}

func (f *heartbeatScopeFixture) receiveScopeObservation() (uint64, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.version, append([]string(nil), f.scopes...)
}
func (f *heartbeatScopeFixture) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	f.mu.Lock()
	if p.Name == "renew_lease" {
		f.renews++
		at := f.widenAt
		if at == 0 {
			at = 3
		}
		if f.renews == at {
			f.version++
			f.scopes = []string{access.MessageReceive, access.SessionRegister}
		}
	}
	if p.Name == "resume_session" {
		f.resumes++
	}
	if p.Name == "wait_for_message" {
		f.receives++
		f.active++
		if f.active > f.maxActive {
			f.maxActive = f.active
		}
		n := f.receives
		s := f.session
		f.mu.Unlock()
		defer func() { f.mu.Lock(); f.active--; f.mu.Unlock() }()
		if n == 1 {
			return &mcp.CallToolResult{StructuredContent: broker.Message{To: s.Name, RegistrationID: s.RegistrationID, Body: "scope restored"}}, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.mu.Unlock()
	return f.Upstream.CallTool(ctx, p)
}

func TestCachedReceiveDenialRestoresFromHeartbeatWithoutModelCall(t *testing.T) {
	inner := newSwitchableUpstream(t)
	f := &heartbeatScopeFixture{Upstream: inner, version: 1, scopes: []string{access.SessionRegister}}
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := (durableOptions{state: state, scope: "scopes", heartbeat: 10 * time.Millisecond, timeout: time.Second, backoff: func(int) time.Duration { return time.Millisecond }, receiverIdentity: fixtureReceiverIdentity{}}).withDefaults()
	id := broker.AttachRequest{Name: "scope-worker", Harness: "codex", SessionID: "11111111-1111-4111-8111-111111115501", PID: 5501, CWD: "/synthetic", ChannelID: "initial-lease"}
	b := &ingressBinding{mu: &sync.Mutex{}, identity: id, durable: &durableRegistration{key: clientstate.RegistrationKey("scopes", "codex", id.SessionID), clientID: "scope-client", secret: clientstate.NewSecret()}}
	u := opts.wrapReceive(f, "synthetic-session")
	u.(*receiveUpstream).permission.decide(receiveForbidden)
	session, err := registerDurable(t.Context(), u, b, opts, id)
	if err != nil {
		t.Fatal("fixture register", err)
	}
	f.mu.Lock()
	f.session = session
	f.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	delivered := make(chan struct{}, 1)
	go func() {
		defer close(done)
		runDurableReceiver(ctx, u, b, opts, func(context.Context, broker.Message) error { delivered <- struct{}{}; return nil })
	}()
	defer func() { cancel(); <-done }()
	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Fatal("cached denial never resumed reception after successful renew widened scopes")
	}
	f.mu.Lock()
	receives, resumes := f.receives, f.resumes
	f.mu.Unlock()
	if receives < 1 || resumes != 1 {
		t.Fatalf("receive restoration must refresh one lease exactly once: receives=%d resumes=%d", receives, resumes)
	}
}

func TestReceiveScopesRemoveRestoreUnchangedAndNativeGuard(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "native-unavailable"}[unavailable], func(t *testing.T) {
			inner := newSwitchableUpstream(t)
			f := &heartbeatScopeFixture{Upstream: inner, version: 1, scopes: []string{access.SessionRegister}, widenAt: -1}
			state, _ := clientstate.Open(t.TempDir())
			opts := (durableOptions{state: state, scope: "cycle", heartbeat: 10 * time.Millisecond, timeout: time.Second, backoff: func(int) time.Duration { return time.Millisecond }, receiverIdentity: fixtureReceiverIdentity{}}).withDefaults()
			id := broker.AttachRequest{Name: "cycle-worker", Harness: "codex", SessionID: "11111111-1111-4111-8111-111111115502", PID: 5502, CWD: "/synthetic", ChannelID: "cycle-lease"}
			b := &ingressBinding{mu: &sync.Mutex{}, identity: id, receiveUnavailable: unavailable, durable: &durableRegistration{key: clientstate.RegistrationKey("cycle", "codex", id.SessionID), clientID: "cycle-client", secret: clientstate.NewSecret()}}
			u := opts.wrapReceive(f, "cycle-credential")
			session, err := registerDurable(t.Context(), u, b, opts, id)
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			f.session = session
			f.mu.Unlock()
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() {
				defer close(done)
				runDurableReceiver(ctx, u, b, opts, func(context.Context, broker.Message) error { return nil })
			}()
			defer func() { cancel(); <-done }()
			waitFor(t, "denied heartbeat stable", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.renews >= 2 })
			f.mu.Lock()
			before := f.resumes
			f.mu.Unlock()
			set := func(allowed bool) {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.version++
				f.scopes = []string{access.SessionRegister}
				if allowed {
					f.scopes = []string{access.MessageReceive, access.SessionRegister}
				}
			}
			set(true)
			waitFor(t, "startup absent then granted", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.resumes == before+1 })
			if unavailable {
				f.mu.Lock()
				receives := f.receives
				f.mu.Unlock()
				rows, _ := inner.service.ListSessions(t.Context())
				if receives != 0 || len(rows) != 1 || rows[0].AutomaticDeliveryReady {
					t.Fatal("scope grant bypassed native sink guard")
				}
				return
			}
			waitFor(t, "receive started", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.receives > 0 })
			set(false)
			waitFor(t, "removal refreshed pending lease", func() bool {
				rows, _ := inner.service.ListSessions(t.Context())
				return receiveDenied(u) && len(rows) == 1 && !rows[0].AutomaticDeliveryReady
			})
			f.mu.Lock()
			removed := f.resumes
			receives := f.receives
			pendingRenews := f.renews
			f.mu.Unlock()
			waitFor(t, "pending receiver heartbeat stable", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.renews >= pendingRenews+2 })
			set(true)
			waitFor(t, "restoration rejoined old worker", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.resumes == removed+1 && f.receives > receives })
			f.mu.Lock()
			resumes := f.resumes
			renews := f.renews
			f.mu.Unlock()
			set(true)
			waitFor(t, "unchanged scopes renewed", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.renews >= renews+4 })
			f.mu.Lock()
			after := f.resumes
			f.mu.Unlock()
			if after != resumes {
				t.Fatal("unchanged scopes restarted receiver")
			}
			f.mu.Lock()
			maxActive := f.maxActive
			f.mu.Unlock()
			if maxActive > 1 {
				t.Fatal("old receive worker overlapped its replacement")
			}
		})
	}
}
