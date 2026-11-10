package channel

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// Keep the real HTTP transport and all credential/suspension observations.
// Only expose completed resume attempts to synchronize the unsuspend test.
type suspendPollUpstream struct {
	*brokerUpstream
	attempts chan struct{}
}

func (u *suspendPollUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	result, err := u.brokerUpstream.CallTool(ctx, p)
	if p.Name == "resume_session" {
		select {
		case u.attempts <- struct{}{}:
		default:
		}
	}
	return result, err
}

func TestSuspendedRealRuntimeUnsuspendAliveWithinThreeSeconds(t *testing.T) {
	r := newDetachedRuntime(t)
	manager := r.harness(t, t.TempDir(), 931)
	manager.attach(t)
	attempts := make(chan struct{}, 32)
	target := r.harness(t, t.TempDir(), 932, func(opts *durableOptions) {
		opts.backoff = nil // production default, not the accelerated fixture schedule
		original := opts.device.newUpstream
		opts.device.newUpstream = func(token string) Upstream { return &suspendPollUpstream{original(token).(*brokerUpstream), attempts} }
	})
	target.attach(t)
	if err := call(t.Context(), manager.client, "suspend_session", map[string]string{"name": target.session.RegistrationID}, nil); err != nil {
		t.Fatal(err)
	}
	// Let the baseline reach its 4-second exponential interval. The fixed
	// schedule must remain 2 seconds even after repeated suspension responses.
	for i := 0; i < 3; i++ {
		select {
		case <-attempts:
		case <-time.After(10 * time.Second):
			t.Fatal("suspended resume stopped polling")
		}
	}
	if err := call(t.Context(), manager.client, "unsuspend_session", map[string]string{"name": target.session.RegistrationID}, nil); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	waitForWithin(t, 3*time.Second, "unsuspend -> alive", func() bool {
		var current broker.Session
		err := call(t.Context(), manager.client, "get_session", map[string]string{"name": target.session.RegistrationID}, &current)
		return err == nil && current.Status == broker.StatusAlive && current.AutomaticDeliveryReady
	})
	t.Logf("unsuspend -> alive: %s", time.Since(started))
}
