package channel

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type gateCountUpstream struct {
	*brokerUpstream
	registers *atomic.Int32
}

func (u *gateCountUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name == "register_session" {
		u.registers.Add(1)
	}
	return u.brokerUpstream.CallTool(ctx, p)
}

func TestFollowSkipGateRestartResumeKeepsRegistrationAndBothDirections(t *testing.T) {
	for _, failFirst := range []bool{false, true} {
		name := "normal"
		if failFirst {
			name = "first-follow-error"
		}
		t.Run(name, func(t *testing.T) {
			r := newDetachedRuntime(t)
			dir := t.TempDir()
			oldID := claudeHandoverIdentity(901, os.Getpid()+1000)
			var oldBinding *ingressBinding
			old := r.identityHarness(t, dir, oldID, func(o *durableOptions) {
				handoverOptions(o)
				o.bindingCreated = func(b *ingressBinding) { oldBinding = b }
			})
			old.attach(t)
			original := old.session
			_, _, saved := handoverSaved(t, r, dir, oldID)
			oldBinding.mu.Lock()
			lease := oldBinding.identity.ChannelID
			oldBinding.mu.Unlock()
			probe := newTestBrokerUpstream(t.Context(), r.endpoint)
			probe.setBearerToken(saved.Credential.AccessToken)
			if err := call(t.Context(), probe, "report_ingress_failure", map[string]string{"sessionId": oldID.SessionID, "registrationId": original.RegistrationID, "channelId": lease}, nil); err != nil {
				t.Fatal("could not stop P0 ingress")
			}
			probe.Close()
			old.close()
			initial := claudeHandoverIdentity(902, os.Getpid())
			current := oldID
			current.PID = initial.PID
			roster := &followRoster{id: initial}
			clock := &followClock{now: time.Unix(1000, 0), ticks: make(chan followTick, 1)}
			native := followNativeInbox(t, initial)
			var registers atomic.Int32
			var observeUnavailable atomic.Bool
			h, f := unregisteredMonitor(t, r, dir, initial, roster, clock, func(o *durableOptions) {
				o.observe = func(ctx context.Context, id broker.AttachRequest) (broker.AttachRequest, error) {
					if observeUnavailable.Load() {
						return broker.AttachRequest{}, errors.New("synthetic observation unavailable")
					}
					return roster.observe(ctx, id)
				}
				// Current conversation differs from the inherited environment UUID;
				// the real Unix peer is this P1 process.
				o.claudeInboxFactory = newClaudeInboxFactory(t.Context(), current, roster.observe, o.parentProcessKey)
				original := o.device.newUpstream
				o.device.newUpstream = func(token string) Upstream {
					return &gateCountUpstream{original(token).(*brokerUpstream), &registers}
				}
			})
			manager := r.harness(t, t.TempDir(), 9903)
			manager.attach(t)
			before := r.direct.Load()
			var injected bool
			f.mu.Lock()
			startup := f.startup
			f.startup = func(ctx context.Context, id broker.AttachRequest) (broker.AttachRequest, error) {
				next, err := startup(ctx, id)
				if failFirst && err == nil && !injected {
					injected = true
					return next, errors.New("synthetic first follow failure")
				}
				return next, err
			}
			f.mu.Unlock()
			roster.set(current)
			unregisteredStep(t, clock)
			if err := call(t.Context(), manager.client, "send_message", broker.Message{From: manager.session.Name, To: original.Name, Body: "inbound"}, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case message := <-native:
				if message.Body != "inbound" {
					t.Fatal("cross conversation delivery")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("P1 native inbox did not receive")
			}
			if failFirst {
				observeUnavailable.Store(true)
				blocked := h.status(t)
				observeUnavailable.Store(false)
				if blocked.ReceiveError != "reconnect-required" || !strings.Contains(blocked.Message, "follow-skipped: unavailable") || strings.Contains(blocked.Message, "native inbox is unavailable") {
					t.Fatal("follow skip misdiagnosed as an inbox failure")
				}
			}
			s := h.status(t)
			if s.ReceiveError != "" {
				t.Fatalf("inbound works but resumed sender remains blocked: %s", s.ReceiveError)
			}
			if !s.Registered || s.RegistrationID != original.RegistrationID || s.Name != original.Name {
				t.Fatal("resume replaced the registration")
			}
			if err := call(t.Context(), h.client, "send_message", broker.Message{From: s.Name, To: manager.session.Name, Body: "outbound"}, nil); err != nil {
				t.Fatal("resumed sender blocked", err)
			}
			select {
			case message := <-manager.received:
				if message.Body != "outbound" {
					t.Fatal("wrong outbound delivery")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("outbound not received")
			}
			_, _, after := handoverSaved(t, r, dir, current)
			if registers.Load() != 0 || r.direct.Load() != before || saved.Registration.Secret != after.Registration.Secret {
				t.Fatal("resume made a new registration/admission or replaced the proof")
			}
			f.mu.Lock()
			if failFirst && !injected {
				t.Error("first follow error was not injected")
			}
			f.mu.Unlock()
		})
	}
}
