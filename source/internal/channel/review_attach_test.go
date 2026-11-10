package channel

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type reviewAttachUpstream struct {
	*brokerUpstream
	renewals, acknowledgments *atomic.Int32
}

func (u *reviewAttachUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	result, err := u.brokerUpstream.CallTool(ctx, p)
	if p.Name == "renew_lease" && err == nil && !result.IsError {
		raw, _ := json.Marshal(result.StructuredContent)
		var out broker.RegisterResult
		if json.Unmarshal(raw, &out) == nil && out.Code == "" {
			u.renewals.Add(1)
			raw, _ = json.Marshal(p.Arguments)
			var request broker.RenewRequest
			if json.Unmarshal(raw, &request) == nil && request.AckThrough > 0 {
				u.acknowledgments.Add(1)
			}
		}
	}
	return result, err
}

// Port the review overlay into the real loopback fixture and check receive progress.
func TestReviewSelfStopLiveReattachReadyHeartbeatAndAck(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		t.Run(map[bool]string{false: "retained-grant", true: "F1-revoked"}[cleanup], func(t *testing.T) {
			r := newDetachedRuntime(t)
			made := make(chan *ingressBinding, 2)
			var renewals, acknowledgments atomic.Int32
			h := r.harness(t, t.TempDir(), 29101, func(o *durableOptions) {
				o.hookWrites = &hookWriteCompletions{}
				o.bindingCreated = func(b *ingressBinding) { made <- b }
				original := o.device.newUpstream
				o.device.newUpstream = func(token string) Upstream {
					return &reviewAttachUpstream{original(token).(*brokerUpstream), &renewals, &acknowledgments}
				}
			})
			peer := r.harness(t, t.TempDir(), 29102)
			peer.attach(t)
			h.attach(t)
			b := <-made
			old := h.session.RegistrationID
			if err := call(t.Context(), h.client, "stop_session", map[string]string{"name": old}, nil); err != nil {
				t.Fatal("self stop failed")
			}
			if cleanup {
				cleanupPanelList(t, r)
			}
			previousRenewals := renewals.Load()
			h.attach(t)
			select {
			case b = <-made:
			default:
			}
			b.mu.Lock()
			retired := b.retired
			b.mu.Unlock()
			if retired {
				t.Fatal("reattach reused permanently retired binding")
			}
			if (h.session.RegistrationID != old) != cleanup {
				t.Fatal("wrong registration replacement policy")
			}
			waitFor(t, "reattached receiver ready after heartbeat", func() bool {
				s := h.status(t)
				return s.ReceiverReady != nil && *s.ReceiverReady && renewals.Load() > previousRenewals
			})
			var sent broker.SendResult
			if err := call(t.Context(), peer.client, "send_message", map[string]string{"from": peer.session.Name, "to": broker.SelectorRegistration + h.session.RegistrationID, "body": "review-reattach-peer"}, &sent); err != nil {
				t.Fatal("peer send")
			}
			if !sent.Accepted {
				t.Fatal("peer send refused", sent.Code)
			}
			select {
			case m := <-h.received:
				if m.Body != "review-reattach-peer" || m.DeliverySequence == 0 {
					t.Fatal("wrong peer handoff or ACK not negotiated")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("peer message not received")
			}
			waitFor(t, "successful post-reattach ACK renew", func() bool { return acknowledgments.Load() > 0 })
		})
	}
}

type reviewRejectResume struct{ *recordingUpstream }

func (u reviewRejectResume) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name == "resume_session" {
		u.rejected.Store(true)
	}
	return u.recordingUpstream.CallTool(ctx, p)
}
func TestReviewManualExplicitAttachRejectedCredentialKeepsPendingText(t *testing.T) {
	auth := newDeviceAuthServer(t)
	up := newSwitchableUpstream(t)
	made := make(chan *ingressBinding, 1)
	f := newDeviceModeFixture(t, auth, up, t.TempDir(), singleIdentity(), func(f *deviceModeFixture) {
		f.configureDurable = func(o *durableOptions) {
			o.heartbeat = time.Hour
			o.bindingCreated = func(b *ingressBinding) { made <- b }
		}
		f.wrapUpstream = func(_ string, u Upstream) Upstream { return reviewRejectResume{u.(*recordingUpstream)} }
	})
	f.grant("11111111-1111-4111-8111-111111111111", strings.Join([]string{"asb", "s", "review-manual-fixture"}, "_"), "manual-review")
	waitFor(t, "manual registration", func() bool { return f.status("")["registered"] == true })
	b := <-made
	b.mu.Lock()
	cancel, done := b.cancel, b.done
	b.mu.Unlock()
	cancel()
	<-done
	b.mu.Lock()
	b.started, b.failed = false, false
	b.mu.Unlock()
	result := f.call("", "attach_session", map[string]any{})
	if !result.IsError || len(result.Content) != 1 {
		t.Fatal("manual rejected credential should return guidance")
	}
	want := b.durable.session.pendingText()
	if result.Content[0].(*mcp.TextContent).Text != want {
		t.Fatal("manual rejection replaced pendingText with binding retry")
	}
	if f.codeRequests() != 1 {
		t.Fatal("manual rejection unexpectedly requested approval")
	}
}

func TestReviewExplicitAttachDoesNotReviveHandoverRetirement(t *testing.T) {
	r := newDetachedRuntime(t)
	made := make(chan *ingressBinding, 1)
	h := r.harness(t, t.TempDir(), 29111, func(o *durableOptions) { o.bindingCreated = func(b *ingressBinding) { made <- b } })
	h.attach(t)
	b := <-made
	b.mu.Lock()
	b.retired, b.failed, b.selfStopped = true, true, false
	b.durable.handedOver = true
	cancel, done := b.cancel, b.done
	b.mu.Unlock()
	cancel()
	<-done
	result, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
	if err != nil || !result.IsError {
		t.Fatal("handover-retired source was allowed to attach")
	}
	b.mu.Lock()
	retired := b.retired
	b.mu.Unlock()
	if !retired {
		t.Fatal("handover retirement fence cleared")
	}
}
