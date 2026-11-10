package channel

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type retiredReceiverUpstream struct {
	entered chan struct{}
	revoked chan struct{}
	resumes atomic.Int32
}

func (u *retiredReceiverUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	switch p.Name {
	case "wait_for_message":
		close(u.entered)
		select {
		case <-u.revoked:
			return nil, errors.New("authorization refused")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case "resume_session":
		u.resumes.Add(1)
	}
	return nil, errors.New("authorization refused")
}
func TestHandoverRetiredSourceDoesNotReconnect(t *testing.T) {
	store, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := &ingressBinding{mu: &sync.Mutex{}, identity: broker.AttachRequest{Harness: "claude", SessionID: "11111111-1111-4111-8111-111111111111", Name: "worker"}, durable: &durableRegistration{key: clientstate.RegistrationKey("scope", "claude", "source"), registrationID: "ABCDEFGHIJKLMNOPQRSTUVWXYZ"}}
	opts := durableOptions{state: store, heartbeat: time.Hour, timeout: time.Second, backoff: func(int) time.Duration { return time.Millisecond }}
	u := &retiredReceiverUpstream{entered: make(chan struct{}), revoked: make(chan struct{})}
	done := make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	go func() {
		defer close(done)
		runDurableReceiver(ctx, u, b, opts, func(context.Context, broker.Message) error { return nil })
	}()
	<-u.entered
	// The follow handover marks the source retired before its exchange revokes
	// the old credential. Let that revocation terminate the receive stream.
	b.mu.Lock()
	b.retired = true
	b.mu.Unlock()
	close(u.revoked)
	<-done
	if n := u.resumes.Load(); n != 0 {
		t.Fatalf("retired handover source attempted %d reconnects; want zero", n)
	}
	diagnostics, err := store.LoadDiagnostics(b.durable.key)
	if err != nil {
		t.Fatal(err)
	}
	if hasDiagnostic(diagnostics, "reconnect-failed", "") {
		t.Fatal("retired source emitted reconnect-failed")
	}
}

// Exercise the real local follow coordinator and HTTP credential revocation,
// as well as the deterministic source-exit test above.
type handoverCountUpstream struct {
	*brokerUpstream
	resumes *atomic.Int32
}

func (u *handoverCountUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name == "resume_session" {
		u.resumes.Add(1)
	}
	return u.brokerUpstream.CallTool(ctx, p)
}
func TestHandoverSourceHTTPReceiverRetiresWithoutReconnect(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	c0 := claudeHandoverIdentity(951, 9101)
	c1 := claudeHandoverIdentity(952, 9101)
	roster := &followRoster{id: c0}
	var connections, resumes atomic.Int32
	var source *ingressBinding
	var sourceUpstream Upstream
	h := r.identityHarness(t, dir, c0, func(o *durableOptions) {
		followOptions(roster)(o)
		o.connected = func(u Upstream) {
			if sourceUpstream == nil {
				sourceUpstream = u
			}
		}
		o.device.authorizer = &handoverAdviceAuthorizer{o.device.authorizer.(*deviceAuthClient), func() {
			// Force the rejected-source advice after server apply and retirement,
			// before destination publication can cancel/join the source receiver.
			_ = upstreamCredentialGuidance(sourceUpstream, true)
		}}
		o.bindingCreated = func(b *ingressBinding) {
			if source == nil {
				source = b
			}
		}
		original := o.device.newUpstream
		o.device.newUpstream = func(token string) Upstream {
			u := original(token).(*brokerUpstream)
			if connections.Add(1) == 1 {
				return &handoverCountUpstream{u, &resumes}
			}
			return u
		}
	})
	h.attach(t)
	before := resumes.Load()
	roster.set(c1)
	waitFor(t, "destination attached", func() bool { return h.status(t).HandoverFrom != nil })
	source.mu.Lock()
	done := source.done
	source.mu.Unlock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handover source receiver did not stop")
	}
	if resumes.Load() != before {
		t.Fatal("handover source resumed its revoked credential")
	}
	store, _, _ := handoverSaved(t, r, dir, c0)
	diagnostics, err := store.LoadDiagnostics(source.durable.key)
	if err != nil {
		t.Fatal(err)
	}
	if hasDiagnostic(diagnostics, "reconnect-failed", "") {
		t.Fatal("handover source emitted reconnect-failed")
	}
	manager := r.harness(t, t.TempDir(), 9102)
	manager.attach(t)
	if err := call(t.Context(), manager.client, "send_message", broker.Message{From: manager.session.Name, To: h.session.Name, Body: "after-handover"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.received:
	case <-time.After(3 * time.Second):
		t.Fatal("destination delivery failed")
	}
}

func TestHandoverSourceCredentialAdviceRetired(t *testing.T) {
	for _, state := range []string{"retired", "handed-over"} {
		t.Run(state, func(t *testing.T) {
			r := newDetachedRuntime(t)
			dir := t.TempDir()
			var binding *ingressBinding
			var upstream Upstream
			h := r.harness(t, dir, 9911, func(o *durableOptions) {
				o.bindingCreated = func(b *ingressBinding) { binding = b }
				o.connected = func(u Upstream) { upstream = u }
			})
			h.attach(t)
			binding.mu.Lock()
			if state == "retired" {
				binding.retired = true
			} else {
				binding.durable.handedOver = true
			}
			a := binding.durable.session
			key := binding.durable.key
			binding.mu.Unlock()
			a.mu.Lock()
			credential := a.state.Credential.AccessToken
			a.mu.Unlock()
			_ = upstreamCredentialGuidance(upstream, true)
			store, err := clientstate.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			diagnostics, err := store.LoadDiagnostics(key)
			if err != nil {
				t.Fatal(err)
			}
			if hasDiagnostic(diagnostics, "reconnect-failed", "") {
				t.Fatal("retired source advice emitted reconnect-failed")
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.state.Credential == nil || a.state.Credential.AccessToken != credential || a.recovering {
				t.Fatal("retired source advice rejected credential or started recovery")
			}
		})
	}
}

type handoverAdviceAuthorizer struct {
	*deviceAuthClient
	after func()
}

func (a *handoverAdviceAuthorizer) exchange(ctx context.Context, request directCodeRequest) (directGrant, error) {
	grant, err := a.deviceAuthClient.exchange(ctx, request)
	if err == nil && request.Handover {
		a.after()
	}
	return grant, err
}
