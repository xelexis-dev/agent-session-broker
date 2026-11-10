package channel

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestRegisteredConversationSuppressesStaleDetachedGuidance(t *testing.T) {
	for _, device := range []bool{false, true} {
		name := "configured"
		if device {
			name = "device"
		}
		t.Run(name, func(t *testing.T) {
			bindings := make(chan *ingressBinding, 1)
			up := newSwitchableUpstream(t)
			var status func() map[string]any
			if device {
				f := newDeviceModeFixture(t, newDeviceAuthServer(t), up, t.TempDir(), singleIdentity(), func(f *deviceModeFixture) {
					f.configureDurable = func(opts *durableOptions) { opts.bindingCreated = func(b *ingressBinding) { bindings <- b } }
				})
				f.grant("11111111-1111-4111-8111-111111111111", "asb_s_synthetic-stale", "stale-worker")
				status = func() map[string]any { return f.status("") }
				waitFor(t, "device registered", func() bool { return status()["registered"] == true })
			} else {
				up.service.SetAdmission(grantAdmission{name: "stale-worker"})
				f := newDurableFixtureWithPolicy(t, up, t.TempDir(), true, func(opts *durableOptions, _ *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) {
					opts.bindingCreated = func(b *ingressBinding) { bindings <- b }
				})
				if _, result := f.attach(t, ""); result.IsError {
					t.Fatal("configured attach failed")
				}
				status = func() map[string]any {
					var out map[string]any
					if err := call(t.Context(), f.local, "auth_status", map[string]any{}, &out); err != nil {
						t.Fatal(err)
					}
					return out
				}
			}
			b := <-bindings
			b.mu.Lock()
			b.resumeStopReason = "registration-detached"
			b.mu.Unlock()
			out := status()
			message, _ := out["message"].(string)
			if out["registered"] != true || strings.Contains(message, detachedGuidance) {
				t.Fatal("active registration showed stale detached guidance")
			}
		})
	}
}

func TestDetachedStatusMessagePreservesConcreteReason(t *testing.T) {
	original := "automatic-reauthorization-stopped: registration-detached. existing detail"
	got := detachedStatusMessage(original)
	if !strings.HasPrefix(got, original) || !strings.Contains(got, detachedGuidance) || detachedStatusMessage(got) != got {
		t.Fatal("detached guidance replaced or duplicated the original reason")
	}
}
