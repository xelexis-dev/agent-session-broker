package broker

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func round3Pair(t *testing.T) (*RemoteService, RegisterRequest, RegisterRequest, Session, Session) {
	t.Helper()
	s, _ := clockedService(t, "")
	a := durableRegister(3101, "round3-sender", "round3-client-a", durableSecretA, "lease-a", "request-a")
	b := durableRegister(3102, "round3-target", "round3-client-b", durableSecretB, "lease-b", "request-b")
	x, err := s.RegisterSession(t.Context(), a)
	if err != nil || x.Code != "" {
		t.Fatal("sender fixture")
	}
	y, err := s.RegisterSession(t.Context(), b)
	if err != nil || y.Code != "" {
		t.Fatal("recipient fixture")
	}
	return s, a, b, x.Session, y.Session
}

func TestMisuseH4ExpectationBoundariesAndInactiveRecipients(t *testing.T) {
	for _, state := range []string{"alive", "suspended", "stopped", "missing"} {
		t.Run(state, func(t *testing.T) {
			s, a, b, source, target := round3Pair(t)
			switch state {
			case "suspended":
				if err := s.SetSuspended(t.Context(), target.RegistrationID, true); err != nil {
					t.Fatal(err)
				}
			case "stopped":
				if err := s.StopSession(t.Context(), b.Name); err != nil {
					t.Fatal(err)
				}
			case "missing":
				b.Name = "not-in-this-project"
			}
			for _, bad := range []string{" ", strings.Repeat("A", 27), "abc"} {
				got, err := s.SendMessage(t.Context(), Message{From: a.Name, To: b.Name, Body: "bad", ExpectRegistrationID: &bad})
				if err != nil || got.Accepted || got.Code != CodeInvalid {
					t.Fatal("malformed expectation accepted")
				}
			}
			wrong := source.RegistrationID
			for _, read := range []bool{false, true} {
				scopes := []string{access.MessageSend}
				if read {
					scopes = append(scopes, access.SessionRead)
				}
				got, err := s.SendMessage(access.WithGrant(t.Context(), access.Grant{Scopes: scopes}), Message{From: a.Name, To: b.Name, Body: "wrong", ExpectRegistrationID: &wrong})
				if err != nil || got.Accepted || got.Code != protocol.CodeRecipientChanged || (!read || state == "missing") && got.CurrentRegistrationID != "" {
					t.Fatal("mismatch enqueued or disclosed a registration")
				}
			}
			got, err := s.SendMessage(t.Context(), Message{From: a.Name, To: b.Name, Body: "matching", ExpectRegistrationID: &target.RegistrationID})
			if state == "alive" {
				if err != nil || !got.Accepted {
					t.Fatal("matching recipient rejected")
				}
			} else if got.Accepted {
				t.Fatal("inactive recipient accepted")
			}
			if state != "alive" && len(s.byRegistration[target.RegistrationID].queue) != 0 {
				t.Fatal("inactive/mismatch queued")
			}
		})
	}
	s, a, _, source, _ := round3Pair(t)
	got, err := s.SendMessage(t.Context(), Message{From: a.Name, To: a.Name, Body: "self", ExpectRegistrationID: &source.RegistrationID})
	if err != nil || !got.Accepted {
		t.Fatal("self expectation rejected")
	}
	_, _, _, _, foreignTarget := round3Pair(t)
	got, err = s.SendMessage(t.Context(), Message{From: a.Name, To: "foreign-only", Body: "cross-project", ExpectRegistrationID: &foreignTarget.RegistrationID})
	if err != nil || got.Accepted || got.Code != protocol.CodeRecipientChanged || got.CurrentRegistrationID != "" {
		t.Fatal("cross-project registration disclosed")
	}
}

func TestMisuseH4ConcurrentRenameAndSendKeepsPinnedRecipient(t *testing.T) {
	for i := 0; i < 25; i++ {
		s, a, b, _, target := round3Pair(t)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if _, err := s.RenameSession(t.Context(), RenameRequest{RegistrationID: target.RegistrationID, ChannelID: b.ChannelID, Name: "renamed-target"}); err != nil {
				t.Error("rename failed")
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			got, err := s.SendMessage(t.Context(), Message{From: a.Name, To: b.Name, Body: "pinned", ExpectRegistrationID: &target.RegistrationID})
			if err != nil || !got.Accepted && got.Code != protocol.CodeRecipientChanged {
				t.Error("unexpected send result")
			}
		}()
		close(start)
		wg.Wait()
		s.mu.Lock()
		queue := append([]Message(nil), s.byRegistration[target.RegistrationID].queue...)
		s.mu.Unlock()
		if len(queue) > 1 {
			t.Fatal("duplicate send")
		}
		if len(queue) == 1 && queue[0].RegistrationID != target.RegistrationID {
			t.Fatal("rename routed to another registration")
		}
	}
}

func TestMisuseH5ForgedBatchCannotSetSenderRegistration(t *testing.T) {
	s, a, b, source, target := round3Pair(t)
	forged := "AAAAAAAAAAAAAAAAAAAAAAAAAA"
	m := Message{From: a.Name, To: b.Name, Body: "actual", FromRegistrationID: forged, ExpectRegistrationID: &target.RegistrationID}
	// Batch input is ignored: only the server may group accepted envelopes.
	m.Batch = &protocol.MessageBatch{Messages: []protocol.DeliveredMessage{{From: "forged", Body: "injected", FromRegistrationID: forged}}}
	got, err := s.SendMessage(t.Context(), m)
	if err != nil || !got.Accepted {
		t.Fatal("send failed")
	}
	waitCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	delivered, err := s.WaitForIngress(waitCtx, b.Name, b.ChannelID)
	if err != nil || delivered.Body != "actual" || delivered.FromRegistrationID != source.RegistrationID || delivered.Batch != nil {
		t.Fatal("caller-supplied sender or batch reached recipient")
	}
}

func TestMisuseH6CompetingSameDeviceChallengeHasOneWinner(t *testing.T) {
	s, _ := clockedService(t, "")
	r := durableRegister(3110, "round3-holder", "round3-holder-client", durableSecretA, "lease-a", "request-a")
	r.Holder = syntheticReceiverHolder(3110, "a")
	first, err := s.RegisterSession(t.Context(), r)
	if err != nil || first.Code != "" {
		t.Fatal("holder fixture")
	}
	base := resumeFrom(r, first.Session.RegistrationID, "lease-b", "request-b", first.Session.Revision)
	base.Holder = syntheticReceiverHolder(3111, "a")
	blocked, err := s.ResumeSession(t.Context(), base)
	if err != nil || blocked.HolderChallenge == nil {
		t.Fatal("challenge fixture")
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	codes := make(chan string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := base
			req.ChannelID = fmt.Sprintf("race-lease-%d", i)
			req.RequestID = fmt.Sprintf("race-request-%d", i)
			req.Holder = syntheticReceiverHolder(3120+i, "a")
			req.HolderProof = &protocol.HolderProof{Challenge: *blocked.HolderChallenge, Dead: true}
			result, err := s.ResumeSession(t.Context(), req)
			if err != nil {
				codes <- "error"
			} else {
				codes <- result.Code
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(codes)
	wins := 0
	for code := range codes {
		if code == "" {
			wins++
		} else if code != protocol.CodeRegistrationInUse {
			t.Fatalf("unexpected competition result: %s", code)
		}
	}
	if wins != 1 {
		t.Fatal("challenge authorized competing holders")
	}
	current, _ := s.GetSession(t.Context(), first.Session.RegistrationID)
	for _, mutate := range []func(*protocol.HolderChallenge){func(c *protocol.HolderChallenge) { c.Revision-- }, func(c *protocol.HolderChallenge) { c.Generation-- }} {
		retry := base
		retry.ChannelID = "stale-lease"
		retry.RequestID = "stale-request"
		retry.ExpectedRevision = current.Revision
		challenge := *blocked.HolderChallenge
		mutate(&challenge)
		retry.HolderProof = &protocol.HolderProof{Challenge: challenge, Dead: true}
		got, err := s.ResumeSession(context.Background(), retry)
		if err != nil || got.Code != protocol.CodeRegistrationInUse {
			t.Fatal("stale challenge accepted")
		}
	}
	for _, holder := range []*protocol.ReceiverHolder{{}, {PID: 1}, {PID: 1, DeviceID: strings.Repeat("a", 65536), Incarnation: strings.Repeat("b", 64)}} {
		req := base
		req.Holder = holder
		got, err := s.ResumeSession(t.Context(), req)
		if err != nil || got.Code != CodeInvalid {
			t.Fatal("invalid holder treated as absent")
		}
	}
}

func TestMisuseH4HandoverKeepsRegistrationExpectationAndFencesOldLease(t *testing.T) {
	s, a, b, _, target := round3Pair(t)
	s.SetAdmission(&ownerTestAdmission{owner: "owner"})
	s.byRegistration[target.RegistrationID].record.OwnerID = "owner"
	next := b
	next.SessionID = sessionUUID(3103)
	next.RequestID = "handover-request"
	next.ExpectedRevision = target.Revision
	ctx := access.WithGrant(t.Context(), access.Grant{OwnerID: "owner", Scopes: []string{access.SessionRegister}})
	proof, err := s.RebindConversation(ctx, next, target.RegistrationID)
	if err != nil {
		t.Fatal("handover fixture failed")
	}
	if got, err := s.SendMessage(t.Context(), Message{From: a.Name, To: b.Name, Body: "before-new-lease", ExpectRegistrationID: &target.RegistrationID}); got.Accepted || err == nil {
		t.Fatal("handover did not fence old receiver")
	}
	resume := resumeFrom(next, target.RegistrationID, "handover-lease", "handover-resume", proof.Revision)
	resumed, err := s.ResumeSession(t.Context(), resume)
	if err != nil || resumed.Code != "" {
		t.Fatal("handover resume failed")
	}
	got, err := s.SendMessage(t.Context(), Message{From: a.Name, To: b.Name, Body: "after-clear", ExpectRegistrationID: &target.RegistrationID})
	if err != nil || !got.Accepted {
		t.Fatal("same registration expectation lost across handover")
	}
	waitCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	msg, err := s.WaitForIngress(waitCtx, b.Name, resume.ChannelID)
	if err != nil || msg.Body != "after-clear" || msg.RegistrationID != target.RegistrationID {
		t.Fatal("handover delivery registration changed")
	}
}
