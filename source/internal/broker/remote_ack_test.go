package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAckLeaseResumeRedeliversUnacked(t *testing.T) {
	s, _ := clockedService(t, "")
	req := durableRegister(1, "ack-peer", "ack-client-0001", durableSecretA, "lease-1", "reg-1")
	req.Capabilities = []string{"message.ack.v1", "message.sentAt"}
	registered, err := s.RegisterSession(t.Context(), req)
	if err != nil || registered.Code != "" {
		t.Fatal(registered, err)
	}
	if _, err := s.SendMessage(t.Context(), Message{From: req.Name, To: req.Name, Body: "m1"}); err != nil {
		t.Fatal(err)
	}
	first, err := s.WaitForIngress(t.Context(), req.Name, req.ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	resume := resumeFrom(req, registered.Session.RegistrationID, "lease-2", "resume-2", registered.Session.Revision)
	resume.Capabilities = req.Capabilities
	next, err := s.ResumeSession(t.Context(), resume)
	if err != nil || next.Code != "" {
		t.Fatal(next, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	again, err := s.WaitForIngress(ctx, req.Name, resume.ChannelID)
	if err != nil {
		t.Fatalf("unacked message not redelivered after new lease: %v", err)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(again)
	var fields map[string]any
	_ = json.Unmarshal(b, &fields)
	if string(a) != string(b) || fields["deliverySequence"] != float64(1) || first.SentAt.IsZero() {
		t.Fatal("redelivery lost sequence/time or changed payload")
	}
}

func ackFixture(t *testing.T) (*RemoteService, *time.Time, RegisterRequest, RegisterResult) {
	t.Helper()
	s, now := clockedService(t, "")
	r := durableRegister(1, "recipient", "ack-fixture-client", durableSecretA, "ack-lease", "ack-register")
	r.Capabilities = []string{"message.ack.v1", "message.sentAt"}
	out, err := s.RegisterSession(t.Context(), r)
	if err != nil || out.Code != "" || out.MessageAckVersion != 1 {
		t.Fatal("ack registration", out.Code, err)
	}
	return s, now, r, out
}
func ackWait(t *testing.T, s *RemoteService, r RegisterRequest, out RegisterResult, cursor, through uint64) Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	m, err := s.WaitForIngressAck(ctx, protocol.IngressRequest{Recipient: r.Name, RegistrationID: out.Session.RegistrationID, ChannelID: r.ChannelID, Epoch: out.Session.Epoch, Cursor: cursor, AckThrough: through})
	if err != nil {
		t.Fatal("ack wait", err)
	}
	return m
}
func ackResume(t *testing.T, s *RemoteService, r RegisterRequest, out RegisterResult, n int) (RegisterRequest, RegisterResult) {
	t.Helper()
	lease := fmt.Sprintf("ack-lease-%d", n)
	resume := resumeFrom(r, out.Session.RegistrationID, lease, fmt.Sprintf("resume-%d", n), out.Session.Revision)
	resume.Capabilities = r.Capabilities
	next, err := s.ResumeSession(t.Context(), resume)
	if err != nil || next.Code != "" {
		t.Fatal("ack resume", next.Code, err)
	}
	r.ChannelID = lease
	return r, next
}
func TestAckWindowReplayAndReconnectBudget(t *testing.T) {
	s, _, r, out := ackFixture(t)
	for i := 0; i < 32; i++ {
		expiryQueueSend(t, s, fmt.Sprintf("m%d", i))
	}
	var cursor uint64
	for i := 0; i < 32; i++ {
		m := ackWait(t, s, r, out, cursor, 0)
		cursor = m.DeliverySequence
	}
	e := s.byRegistration[out.Session.RegistrationID]
	bytes := e.inflightBytes
	for i := 0; i < 10; i++ {
		if m := ackWait(t, s, r, out, 0, 0); m.DeliverySequence != 1 {
			t.Fatal("cursor replay skipped first")
		}
	}
	if len(e.inflight) != 32 || e.inflightBytes != bytes {
		t.Fatal("replay double counted window")
	}
	for i := 32; i < 64; i++ {
		expiryQueueSend(t, s, fmt.Sprintf("m%d", i))
	}
	for n := 0; n < 5; n++ {
		r, out = ackResume(t, s, r, out, n)
		if len(e.queue) != 64 || len(e.inflight) != 0 || len(e.unacked) != 32 {
			t.Fatal("reconnect lost or grew budget")
		}
		if _, err := s.SendMessage(t.Context(), Message{From: r.Name, To: r.Name, Body: "overflow"}); err != ErrQueueFull {
			t.Fatal("requeued overflow admitted another message")
		}
		var cur uint64
		for i := 0; i < 32; i++ {
			m := ackWait(t, s, r, out, cur, 0)
			cur = m.DeliverySequence
			if m.Body != fmt.Sprintf("m%d", i) {
				t.Fatal("requeue order")
			}
		}
	}
	// A full window waits until a renew acknowledges a prefix.
	waitCtx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if _, err := s.WaitForIngressAck(waitCtx, protocol.IngressRequest{Recipient: r.Name, RegistrationID: out.Session.RegistrationID, ChannelID: r.ChannelID, Epoch: out.Session.Epoch, Cursor: 32}); err != context.DeadlineExceeded {
		t.Fatal("full window did not backpressure")
	}
	renewed, err := s.RenewLease(t.Context(), RenewRequest{RegistrationID: out.Session.RegistrationID, ChannelID: r.ChannelID, Epoch: out.Session.Epoch, AckThrough: 32})
	if err != nil || renewed.Code != "" || len(e.inflight) != 0 || len(e.unacked) != 0 || e.ackedThrough != 32 {
		t.Fatal("prefix ack did not release window")
	}
	if m := ackWait(t, s, r, out, 32, 32); m.Body != "m32" {
		t.Fatal("ack wait selected wrong next result")
	}
}
func TestAckWindowByteLimitAndAtomicValidation(t *testing.T) {
	s, _, r, out := ackFixture(t)
	for i := 0; i < 7; i++ {
		expiryQueueSend(t, s, strings.Repeat("x", 11000))
	}
	for i := uint64(0); i < 5; i++ {
		ackWait(t, s, r, out, i, 0)
	}
	e := s.byRegistration[out.Session.RegistrationID]
	if e.inflightBytes > ackWindowBytes || len(e.inflight) != 5 {
		t.Fatal("byte budget")
	}
	base := protocol.IngressRequest{Recipient: r.Name, RegistrationID: out.Session.RegistrationID, ChannelID: r.ChannelID, Epoch: out.Session.Epoch, Cursor: 5, AckThrough: 5}
	for _, mode := range []string{"cursor", "future-ack", "epoch", "registration", "lease"} {
		bad := base
		switch mode {
		case "cursor":
			bad.Cursor = 6
		case "future-ack":
			bad.AckThrough = 6
		case "epoch":
			bad.Epoch = "old-epoch"
		case "registration":
			bad.RegistrationID = "other"
		case "lease":
			bad.ChannelID = "fenced"
		}
		if _, err := s.WaitForIngressAck(t.Context(), bad); err == nil {
			t.Fatal("invalid ack accepted", mode)
		}
		if len(e.inflight) != 5 || len(e.unacked) != 5 || e.ackedThrough != 0 {
			t.Fatal("invalid cursor/ack committed a prefix", mode)
		}
	}
	renew := RenewRequest{RegistrationID: out.Session.RegistrationID, ChannelID: r.ChannelID, Epoch: out.Session.Epoch, AckThrough: 6}
	until := e.until
	if result, err := s.RenewLease(t.Context(), renew); err != nil || result.Code != CodeInvalid || !e.until.Equal(until) || len(e.inflight) != 5 {
		t.Fatal("future renew ack mutated lease")
	}
	if m := ackWait(t, s, r, out, 5, 5); m.DeliverySequence != 6 || len(e.inflight) != 1 {
		t.Fatal("valid atomic ack/next failed")
	}
}
func TestAckDowngradeFixedDeadlineAndPurgeOrders(t *testing.T) {
	for _, order := range []string{"purge-first", "resume-first"} {
		t.Run(order, func(t *testing.T) {
			s, now, r, out := ackFixture(t)
			expiryQueueSend(t, s, "protected")
			first := ackWait(t, s, r, out, 0, 0)
			*now = now.Add(remoteLeaseDuration)
			s.sweep()
			e := s.byRegistration[out.Session.RegistrationID]
			deadline := e.ackProtectedUntil
			if len(e.inflight) != 0 || len(e.unacked) != 1 || len(e.queue) != 1 {
				t.Fatal("issued flag lost on expiry requeue")
			}
			legacy := resumeFrom(r, out.Session.RegistrationID, "legacy-lease", "legacy-resume", out.Session.Revision)
			for i := 0; i < 3; i++ {
				old, err := s.ResumeSession(t.Context(), legacy)
				if err != nil || old.Code != CodeStorage || old.Warning == "" {
					t.Fatal("old client not given retryable protection")
				}
				// Ack-capable reconnect and new arrivals must not restart the original deadline.
				r, out = ackResume(t, s, r, out, i+10)
				expiryQueueSend(t, s, fmt.Sprintf("new-%d", i))
				*now = now.Add(time.Second)
				if !e.ackProtectedUntil.Equal(deadline) {
					t.Fatal("protection deadline extended")
				}
				legacy.ExpectedRevision = out.Session.Revision
			}
			*now = deadline
			if order == "purge-first" {
				s.sweep()
			}
			old, err := s.ResumeSession(t.Context(), legacy)
			if err != nil || old.Code != "" || old.MessageAckVersion != 0 {
				t.Fatal("legacy did not resume at protection deadline", old.Code, err)
			}
			if order == "resume-first" {
				s.sweep()
			}
			m, err := s.WaitForIngress(t.Context(), r.Name, legacy.ChannelID)
			if err != nil || m.Body != first.Body || !m.SentAt.IsZero() || m.DeliverySequence != 0 {
				t.Fatal("protection purge lost legacy payload/projection", err)
			}
		})
	}
}
func TestAckIngressFailureHoldsAndTerminalTransitionsDiscard(t *testing.T) {
	for _, mode := range []string{"failure", "detach", "suspend", "identity-invalid"} {
		t.Run(mode, func(t *testing.T) {
			s, now, r, out := ackFixture(t)
			expiryQueueSend(t, s, "issued")
			ackWait(t, s, r, out, 0, 0)
			expiryQueueSend(t, s, "queued")
			e := s.byRegistration[out.Session.RegistrationID]
			switch mode {
			case "failure":
				if err := s.ReportIngressFailure(t.Context(), r.Name, r.ChannelID); err != nil {
					t.Fatal(err)
				}
			case "detach":
				if err := s.StopSession(t.Context(), r.Name); err != nil {
					t.Fatal(err)
				}
			case "suspend":
				s.mu.Lock()
				err := s.setSuspendedLocked(e, true, nil)
				s.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			case "identity-invalid":
				s.mu.Lock()
				s.retire(e, StatusMissing, "identity-mismatch")
				s.mu.Unlock()
			}
			if mode == "failure" {
				deadline := e.ackProtectedUntil
				*now = now.Add(time.Minute)
				if err := s.ReportIngressFailure(t.Context(), r.Name, r.ChannelID); err != nil {
					t.Fatal(err)
				}
				if len(e.queue) != 2 || len(e.unacked) != 1 || !e.ackProtectedUntil.Equal(deadline) || !e.queueHeldUntil.Equal(deadline) {
					t.Fatal("failure lost or extended hold")
				}
				r, out = ackResume(t, s, r, out, 2)
				if ackWait(t, s, r, out, 0, 0).Body != "issued" {
					t.Fatal("failure requeue order")
				}
			} else if len(e.queue)+len(e.inflight)+len(e.unacked) != 0 {
				t.Fatal("terminal transition retained messages")
			}
		})
	}
}
func TestAckDiscardGapNeverClaimsEarlierSequence(t *testing.T) {
	s, _, r, out := ackFixture(t)
	expiryQueueSend(t, s, "discarded")
	ackWait(t, s, r, out, 0, 0)
	e := s.byRegistration[out.Session.RegistrationID]
	s.mu.Lock()
	a := s.setSuspendedLocked(e, true, nil)
	b := s.setSuspendedLocked(e, false, nil)
	s.mu.Unlock()
	if a != nil || b != nil {
		t.Fatal(a, b)
	}
	out.Session.Revision = e.record.Revision
	r, out = ackResume(t, s, r, out, 2)
	expiryQueueSend(t, s, "later")
	m := ackWait(t, s, r, out, 0, 0)
	renewed, err := s.RenewLease(t.Context(), RenewRequest{RegistrationID: out.Session.RegistrationID, ChannelID: r.ChannelID, Epoch: out.Session.Epoch, AckThrough: m.DeliverySequence})
	if err != nil || renewed.Code != "" || e.ackedThrough != 0 || len(e.inflight) != 0 {
		t.Fatal("later ack implied discarded earlier message")
	}
	raw, _ := json.Marshal(renewed)
	if strings.Contains(string(raw), "ackedThrough") || strings.Contains(string(raw), "deliveredThrough") {
		t.Fatal("sender-visible acknowledgement inference")
	}
}
func TestAckOldNewNegotiationAndRollbackFormat(t *testing.T) {
	for _, mode := range []string{"empty", "never-issued", "issued"} {
		t.Run(mode, func(t *testing.T) {
			s, _, r, out := ackFixture(t)
			if mode != "empty" {
				expiryQueueSend(t, s, "payload")
			}
			if mode == "issued" {
				ackWait(t, s, r, out, 0, 0)
				r, out = ackResume(t, s, r, out, 3)
			}
			legacy := resumeFrom(r, out.Session.RegistrationID, "old-lease", "old-request", out.Session.Revision)
			result, err := s.ResumeSession(t.Context(), legacy)
			if err != nil || (mode == "issued" && result.Code != CodeStorage) || (mode != "issued" && (result.Code != "" || result.MessageAckVersion != 0)) {
				t.Fatal("negotiation matrix", mode, result.Code, err)
			}
		})
	}
	root := t.TempDir()
	s, _ := clockedService(t, root)
	r := durableRegister(1, "recipient", "rollback-ack-client", durableSecretA, "lease", "register")
	r.Capabilities = []string{"message.ack.v1"}
	out, err := s.RegisterSession(t.Context(), r)
	if err != nil || out.Code != "" {
		t.Fatal("persistent register")
	}
	before, err := os.ReadFile(filepath.Join(root, out.Session.RegistrationID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	expiryQueueSend(t, s, "volatile")
	ackWait(t, s, r, out, 0, 0)
	after, err := os.ReadFile(filepath.Join(root, out.Session.RegistrationID+".json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("ack changed persisted registration format")
	}
	var record map[string]any
	if json.Unmarshal(after, &record) != nil {
		t.Fatal("registration decode")
	}
	for key := range record {
		if strings.Contains(strings.ToLower(key), "ack") {
			t.Fatal("persisted ack field")
		}
	}

	reopened, _ := clockedService(t, root)
	e := reopened.byRegistration[out.Session.RegistrationID]
	if e.messageAck || len(e.queue)+len(e.inflight) != 0 || e.session.DeliverySequence != 0 {
		t.Fatal("rollback/restart restored memory delivery state")
	}
}

func TestAckRegisterAndConversationHandoverRequeue(t *testing.T) {
	for _, kind := range []string{"register", "conversation"} {
		t.Run(kind, func(t *testing.T) {
			s, now, r, out := ackFixture(t)
			expiryQueueSend(t, s, "m1")
			first := ackWait(t, s, r, out, 0, 0)
			e := s.byRegistration[out.Session.RegistrationID]
			if kind == "register" {
				r.ChannelID, r.RequestID, r.ExpectedRevision = "register-replacement", "register-again", out.Session.Revision
				next, err := s.RegisterSession(t.Context(), r)
				if err != nil || next.Code != "" {
					t.Fatal("register replacement", next.Code, err)
				}
				out = next
			} else {
				e.record.OwnerID = "fixture-owner"
				r.SessionID = sessionUUID(2)
				r.RequestID = "conversation-handover"
				r.ExpectedRevision = out.Session.Revision
				ctx := access.WithGrant(t.Context(), access.Grant{OwnerID: "fixture-owner", Scopes: []string{access.SessionRegister}})
				proof, err := s.RebindConversation(ctx, r, out.Session.RegistrationID)
				if err != nil {
					t.Fatal("conversation handover", err)
				}
				if len(e.inflight) != 0 || len(e.unacked) != 1 || len(e.queue) != 1 {
					t.Fatal("handover did not requeue")
				}
				deadline := e.ackProtectedUntil
				*now = now.Add(5 * time.Second)
				out.Session.Revision = proof.Revision
				r, out = ackResume(t, s, r, out, 7)
				if !e.ackProtectedUntil.Equal(deadline) {
					t.Fatal("handover deadline restarted at lease issue")
				}
			}
			again := ackWait(t, s, r, out, 0, 0)
			if again.DeliverySequence != first.DeliverySequence || !again.SentAt.Equal(first.SentAt) || again.Body != first.Body {
				t.Fatal("replacement lost original message")
			}
			// Register cannot bypass downgrade protection after inflight is requeued.
			r, out = ackResume(t, s, r, out, 8)
			old := r
			old.Capabilities = nil
			old.RequestID = "old-register"
			old.ChannelID = "old-register-lease"
			old.ExpectedRevision = out.Session.Revision
			if result, err := s.RegisterSession(t.Context(), old); err != nil || result.Code != CodeStorage || result.Warning == "" {
				t.Fatal("register bypassed protected issued flag")
			}
		})
	}
}
func TestAckLeaseCannotSilentlyLoseNegotiation(t *testing.T) {
	s, _, r, out := ackFixture(t)
	retry := r
	retry.Capabilities = nil
	got, err := s.RegisterSession(t.Context(), retry)
	if err != nil || got.Code != "" || got.MessageAckVersion != 1 {
		t.Fatal("same lease replay silently downgraded")
	}
	expiryQueueSend(t, s, "m1")
	ackWait(t, s, r, out, 0, 0)
	_, out = ackResume(t, s, r, out, 9)
	// Fenced late ack must not remove the requeued payload.
	result, err := s.RenewLease(t.Context(), RenewRequest{RegistrationID: out.Session.RegistrationID, ChannelID: retry.ChannelID, Epoch: out.Session.Epoch, AckThrough: 1})
	if err != nil || result.Code != CodeInvalid || len(s.byRegistration[out.Session.RegistrationID].queue) != 1 {
		t.Fatal("fenced ack changed queue")
	}
}

func TestAckProtectionExpiryCannotPurgeLegacyPayload(t *testing.T) {
	for _, purgeFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(purgeFirst), func(t *testing.T) {
			s, now, r, out := ackFixture(t)
			expiryQueueSend(t, s, "retained")
			first := ackWait(t, s, r, out, 0, 0)
			*now = now.Add(remoteLeaseDuration)
			s.sweep()
			e := s.byRegistration[out.Session.RegistrationID]
			if !e.queueHeldUntil.Equal(e.ackProtectedUntil) {
				t.Fatal("fixture does not collide at the two deadlines")
			}
			*now = e.ackProtectedUntil
			if purgeFirst {
				s.sweep()
			}
			legacy := resumeFrom(r, out.Session.RegistrationID, "legacy-after-purge", "legacy-resume", out.Session.Revision)
			legacy.Capabilities = []string{protocol.MessageSentAtCapability}
			result, err := s.ResumeSession(t.Context(), legacy)
			if err != nil || result.Code != "" {
				t.Fatal("deadline blocked legacy")
			}
			if !purgeFirst {
				s.sweep()
			}
			m, err := s.WaitForIngress(t.Context(), r.Name, legacy.ChannelID)
			if err != nil || m.Body != first.Body || !m.SentAt.Equal(first.SentAt) || m.DeliverySequence != 0 {
				t.Fatal("purge destroyed protected legacy payload")
			}
		})
	}
}
func TestAckRetireDiscardsInflightAndQueue(t *testing.T) {
	s, _, r, out := ackFixture(t)
	expiryQueueSend(t, s, "issued")
	ackWait(t, s, r, out, 0, 0)
	expiryQueueSend(t, s, "queued")
	result, err := s.RetireManaged(t.Context(), protocol.RetireRequest{RegistrationID: out.Session.RegistrationID},
		func(Session, string, []Session) (protocol.ManagementEvent, error) {
			return protocol.ManagementEvent{}, nil
		},
		func(_ access.RetirementTarget, detach func() error) ([]protocol.RevokedCredential, error) {
			return nil, detach()
		})
	if err != nil || !result.OK || result.DiscardedMessages != 2 || s.byRegistration[out.Session.RegistrationID] != nil {
		t.Fatal("retire did not discard both pending sets")
	}
}
