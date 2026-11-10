package channel

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// The existing receive wake is the heartbeat's deterministic scheduling boundary;
// real tickers are disabled so advancing this client clock never waits 110s.
type returnRenewClock struct{ at atomic.Int64 }

func (c *returnRenewClock) now() time.Time { return time.Unix(0, c.at.Load()).UTC() }
func (c *returnRenewClock) advance(d time.Duration, u Upstream) {
	c.at.Add(int64(d))
	u.(*receiveUpstream).currentPermission().wake <- struct{}{}
}

type returnRenewUpstream struct {
	*brokerUpstream
	ordinal int32
	block   *atomic.Bool
	started chan struct{}
	inspect *atomic.Bool
	renewed chan broker.RenewRequest
}

func (u *returnRenewUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name == "renew_lease" && u.ordinal == 2 && u.block.Swap(false) {
		close(u.started)
		<-ctx.Done() // The source stream/heartbeat is canceled by the return handover.
		return nil, ctx.Err()
	}
	result, err := u.brokerUpstream.CallTool(ctx, p)
	if p.Name == "renew_lease" && u.ordinal == 3 && u.inspect.Load() && err == nil && result != nil && !result.IsError {
		var request broker.RenewRequest
		raw, _ := json.Marshal(p.Arguments)
		_ = json.Unmarshal(raw, &request)
		select {
		case u.renewed <- request:
		default:
		}
	}
	return result, err
}
func TestReturnHandoverRenewAfter110SecondsKeepsCurrentLease(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	c0 := claudeHandoverIdentity(971, 9201)
	c1 := claudeHandoverIdentity(972, 9201)
	roster := &followRoster{id: c0}
	clock := &returnRenewClock{}
	clock.at.Store(time.Now().UnixNano())
	var block, inspect atomic.Bool
	var connections atomic.Int32
	started := make(chan struct{})
	renewed := make(chan broker.RenewRequest, 1)
	var captured sync.Mutex
	var bindings []*ingressBinding
	var targets []Upstream
	h := r.identityHarness(t, dir, c0, func(o *durableOptions) {
		followOptions(roster)(o)
		o.heartbeat = time.Hour
		o.timeout = 10 * time.Second
		o.device.clock.now = clock.now
		o.bindingCreated = func(b *ingressBinding) { captured.Lock(); bindings = append(bindings, b); captured.Unlock() }
		o.connected = func(u Upstream) { captured.Lock(); targets = append(targets, u); captured.Unlock() }
		original := o.device.newUpstream
		o.device.newUpstream = func(token string) Upstream {
			return &returnRenewUpstream{original(token).(*brokerUpstream), connections.Add(1), &block, started, &inspect, renewed}
		}
	})
	h.attach(t)
	original := h.session
	roster.set(c1)
	waitFor(t, "clear handover", func() bool { s := h.status(t); return s.Registered && s.HandoverFrom != nil })
	captured.Lock()
	sourceTarget := targets[len(targets)-1]
	captured.Unlock()
	block.Store(true)
	clock.advance(0, sourceTarget)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("source heartbeat was not in flight")
	}
	roster.set(c0)
	waitFor(t, "return handover", func() bool {
		s := h.status(t)
		return s.Registered && s.HandoverFrom != nil && s.HandoverFrom.Revision > original.Revision+1
	})
	captured.Lock()
	current := bindings[len(bindings)-1]
	target := targets[len(targets)-1]
	all := append([]*ingressBinding(nil), bindings...)
	captured.Unlock()
	current.mu.Lock()
	lease := current.identity.ChannelID
	generation := current.generation
	revision := current.durable.revision
	current.mu.Unlock()
	resumes := r.resumes.Load()
	beforeRenew := time.Now().UTC()
	inspect.Store(true)
	clock.advance(110*time.Second, target)
	select {
	case request := <-renewed:
		if request.ChannelID != lease || request.RegistrationID != original.RegistrationID {
			t.Fatal("renew reused a stale source lease")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("returned destination did not renew")
	}
	waitFor(t, "renewal diagnostic", func() bool {
		current.mu.Lock()
		key := current.durable.key
		current.mu.Unlock()
		state, _ := clientstate.Open(dir)
		d, e := state.LoadDiagnostics(key)
		return e == nil && d.Summary.LastRenewSuccess.After(beforeRenew)
	})
	current.mu.Lock()
	unchanged := current.generation == generation && current.durable.revision == revision
	current.mu.Unlock()
	if !unchanged || r.resumes.Load() != resumes {
		t.Fatal("renewal caused reconnect or changed revision")
	}
	state, err := clientstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range all {
		binding.mu.Lock()
		key := binding.durable.key
		binding.mu.Unlock()
		d, err := state.LoadDiagnostics(key)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range d.Events {
			if event.Event == "renew-failed" || event.Event == "reconnect-failed" || event.Event == "reconnect-start" {
				t.Fatalf("return handover/late source renewal emitted %s (%s)", event.Event, event.Reason)
			}
		}
	}
}
