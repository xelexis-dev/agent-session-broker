package channel

import (
	"context"
	"errors"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClaudeStartupSettlesBeforeChoosingAuthorizationIdentity(t *testing.T) {
	initial := singleIdentity()["11111111-1111-4111-8111-111111111111"]
	resumed := initial
	initial.SessionID = "22222222-2222-4222-8222-222222222222"
	var observations atomic.Int32
	opts := durableOptions{claudeStartupClock: instantClaudeClock(), device: &deviceAuthOptions{}, observe: func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) {
		if observations.Add(1) == 1 {
			return initial, nil
		}
		return resumed, nil
	}}
	id, err := claudeConversationIdentity(initial, &opts)(t.Context(), nil)
	if err != nil || id.SessionID != resumed.SessionID {
		t.Fatal("startup selected transient identity before stabilization")
	}
}

func TestClaudeNativeResumeBeforeSinkAndDegradedTools(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "automatic", true: "explicit"}[explicit], func(t *testing.T) {
			const sid = "11111111-1111-4111-8111-111111111111"
			auth, up := newDeviceAuthServer(t), newSwitchableUpstream(t)
			dir := t.TempDir()
			first := newDeviceModeFixture(t, auth, up, dir, singleIdentity())
			first.grant(sid, "fixture-credential", "agent-one")
			waitFor(t, "original stored registration", func() bool { return first.lastSavedRegistrationID() != "" })
			original, err := up.session("agent-one")
			if err != nil {
				t.Fatal("original registration missing")
			}
			first.stop()
			beforeCodes := first.codeRequests()
			t.Setenv("CLAUDE_CODE_SESSION_ID", "22222222-2222-4222-8222-222222222222")
			t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "synthetic-only")
			t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "/private/tmp/synthetic-inbox.sock")
			ids := singleIdentity()
			if explicit {
				other := ids[sid]
				other.SessionID = "33333333-3333-4333-8333-333333333333"
				ids[other.SessionID] = other
			}
			var sinkCalls atomic.Int32
			second := newDeviceModeFixture(t, auth, up, dir, ids, func(f *deviceModeFixture) {
				f.deferAttach = true
				f.sinkFactory = func(id broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
					sinkCalls.Add(1)
					if f.log.last("resume_session") != "fixture-credential" {
						t.Error("native sink built before exact stored registration resumed")
					}
					return localIngressSink(id)
				}
			})
			if explicit {
				if r := second.call(sid, "attach_session", map[string]any{}); r.IsError {
					t.Fatal("explicit attach failed instead of resuming degraded registration")
				}
			}
			waitFor(t, "native mismatch retains resumed registration", func() bool {
				s, e := up.session("agent-one")
				return e == nil && s.Revision > original.Revision && s.RegistrationID == original.RegistrationID
			})
			waitFor(t, "degraded auth status", func() bool {
				s := second.status(sid)
				message, _ := s["message"].(string)
				return s["status"] == "authorized" && s["registered"] == true && strings.Contains(message, "reconnect-required") && s["userCode"] == nil
			})
			if sinkCalls.Load() != 1 || second.codeRequests() != beforeCodes {
				t.Fatal("resume created approval or rebuilt unavailable sink")
			}
			if second.call(sid, "list_sessions", map[string]any{}).IsError || second.call(sid, "get_session", map[string]any{"name": "agent-one"}).IsError {
				t.Fatal("degraded read failed")
			}
			for _, tool := range []string{"send_message", "wait_for_message"} {
				r := second.call(sid, tool, map[string]any{})
				if !r.IsError || len(r.Content) != 1 || r.Content[0].(*mcp.TextContent).Text != claudeInboxRecovery {
					t.Fatal("degraded send/receive missing fixed reconnect guidance")
				}
			}
			waitFor(t, "degraded lease renewed", func() bool { return second.log.last("renew_lease") == "fixture-credential" })
			s, e := up.session("agent-one")
			if e != nil || s.AutomaticDeliveryReady || string(s.Status) != "alive" {
				t.Fatal("degraded heartbeat lost registration or advertised reception")
			}
		})
	}
}
func TestClaudeResumeNeverBorrowsAnotherConversationCredential(t *testing.T) {
	const saved = "11111111-1111-4111-8111-111111111111"
	auth, up := newDeviceAuthServer(t), newSwitchableUpstream(t)
	dir := t.TempDir()
	first := newDeviceModeFixture(t, auth, up, dir, singleIdentity())
	first.grant(saved, "fixture-credential", "agent-one")
	waitFor(t, "stored original", func() bool { return first.lastSavedRegistrationID() != "" })
	first.stop()
	beforeCodes := first.codeRequests()
	other := singleIdentity()[saved]
	other.SessionID = "22222222-2222-4222-8222-222222222222"
	second := newDeviceModeFixture(t, auth, up, dir, map[string]broker.AttachRequest{other.SessionID: other})
	waitFor(t, "new identity requests its own approval", func() bool { return second.codeRequests() == beforeCodes+1 })
	if second.log.last("resume_session") != "" || second.log.last("register_session") != "" {
		t.Fatal("another conversation used the saved credential")
	}
	if second.status(other.SessionID)["status"] != "pending" {
		t.Fatal("new conversation inherited authorization")
	}
}

func instantClaudeClock() devicePollClock {
	var mu sync.Mutex
	now := time.Unix(1000, 0)
	return devicePollClock{now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }, sleep: func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
		return nil
	}}
}

func TestClaudeStartupUnstableAndCanceledNeverSelectsIdentity(t *testing.T) {
	initial := singleIdentity()["11111111-1111-4111-8111-111111111111"]
	var calls atomic.Int32
	opts := durableOptions{claudeStartupClock: instantClaudeClock(), device: &deviceAuthOptions{}, observe: func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) {
		id := initial
		id.SessionID = fmt.Sprintf("22222222-2222-4222-8222-%012d", calls.Add(1))
		return id, nil
	}}
	if _, err := claudeConversationIdentity(initial, &opts)(t.Context(), nil); err == nil {
		t.Fatal("unstable startup selected an authorization identity")
	}
	if calls.Load() > 51 {
		t.Fatal("startup observation exceeded its bound")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := claudeConversationIdentity(initial, &opts)(ctx, nil); err == nil {
		t.Fatal("canceled startup selected an identity")
	}
}

func TestClaudeNativePostFailureKeepsDegradedLease(t *testing.T) {
	const sid = "11111111-1111-4111-8111-111111111111"
	auth, up := newDeviceAuthServer(t), newSwitchableUpstream(t)
	f := newDeviceModeFixture(t, auth, up, t.TempDir(), singleIdentity(), func(f *deviceModeFixture) {
		f.sinkFactory = func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
			return func(context.Context, broker.Message) error { return errors.New("synthetic-private-transport-detail") }, nil, nil
		}
	})
	f.grant(sid, "fixture-credential", "agent-one")
	waitFor(t, "native initial readiness", func() bool { s, e := up.session("agent-one"); return e == nil && s.AutomaticDeliveryReady })
	original, _ := up.session("agent-one")
	beforeCodes := f.codeRequests()
	peer := broker.RegisterRequest{Harness: "codex", SessionID: "44444444-4444-4444-8444-444444444444", PID: 4243, CWD: "/synthetic", ClientID: "peer-client-001", Name: "peer", ChannelID: "peer-lease", Secret: "synthetic-resume-secret-0123456789abcdef", RequestID: "peer-register"}
	r, e := up.CallTool(t.Context(), &mcp.CallToolParams{Name: "register_session", Arguments: peer})
	if e != nil || r.IsError {
		t.Fatal("peer fixture unavailable")
	}
	r, e = up.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: broker.Message{From: "peer", To: "agent-one", Body: "synthetic"}})
	if e != nil || r.IsError {
		t.Fatal("fixture message unavailable")
	}
	waitFor(t, "post failure degraded recovery", func() bool {
		s, e := up.session("agent-one")
		return e == nil && s.Generation > original.Generation && !s.AutomaticDeliveryReady && s.RegistrationID == original.RegistrationID
	})
	status := f.status(sid)
	message, _ := status["message"].(string)
	if status["registered"] != true || status["status"] != "authorized" || !strings.Contains(message, "reconnect-required") || strings.Contains(message, "synthetic-private") {
		t.Fatal("post failure exposed private detail or lost authorized registration")
	}
	if f.call(sid, "list_sessions", map[string]any{}).IsError || f.codeRequests() != beforeCodes {
		t.Fatal("post failure blocked tools or issued approval")
	}
	up.restart()
	waitFor(t, "degraded registration survives server restart", func() bool {
		s, e := up.session("agent-one")
		return e == nil && string(s.Status) == "alive" && !s.AutomaticDeliveryReady && s.RegistrationID == original.RegistrationID
	})
}

func TestClaudeHealthySinkReadiesOriginalLeaseWithoutResume(t *testing.T) {
	const sid = "11111111-1111-4111-8111-111111111111"
	auth, up := newDeviceAuthServer(t), newSwitchableUpstream(t)
	f := newDeviceModeFixture(t, auth, up, t.TempDir(), singleIdentity())
	f.grant(sid, "fixture-credential", "agent-one")
	waitFor(t, "healthy original lease ready", func() bool { s, e := up.session("agent-one"); return e == nil && s.AutomaticDeliveryReady })
	session, e := up.session("agent-one")
	if e != nil || session.Generation != 1 || session.Revision != 1 || f.log.last("resume_session") != "" {
		t.Fatal("healthy sink replaced the original registration lease instead of activating it")
	}
	if f.call(sid, "send_message", broker.Message{From: "agent-one", To: "agent-one", Body: "original-lease"}).IsError {
		t.Fatal("healthy original lease cannot send")
	}
	select {
	case m := <-f.sinks[sid]:
		if m.Body != "original-lease" {
			t.Fatal("healthy receiver mismatch")
		}
	case <-time.After(time.Second):
		t.Fatal("healthy original lease has no receiver")
	}
}
