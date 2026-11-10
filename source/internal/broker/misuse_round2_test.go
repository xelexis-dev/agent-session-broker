package broker

import (
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func TestMisuseD1ConcurrentRenameHasOneWinner(t *testing.T) {
	for iteration := 0; iteration < 20; iteration++ {
		s, _ := clockedService(t, "")
		requests := []RegisterRequest{durableRegister(1, "alpha", "client-alpha", durableSecretA, "lease-a", "request-a"), durableRegister(2, "beta", "client-bravo", durableSecretB, "lease-b", "request-b")}
		sessions := make([]Session, 2)
		for i, r := range requests {
			out, err := s.RegisterSession(t.Context(), r)
			if err != nil || out.Code != "" {
				t.Fatal("fixture failed")
			}
			sessions[i] = out.Session
		}
		start := make(chan struct{})
		results := make(chan int, 2)
		for i := range requests {
			go func(i int) {
				<-start
				_, err := s.RenameSession(t.Context(), RenameRequest{RegistrationID: sessions[i].RegistrationID, ChannelID: requests[i].ChannelID, Name: "contended"})
				if err == nil {
					results <- i
				} else {
					results <- (i + 2)
				}
			}(i)
		}
		close(start)
		wins := 0
		for range requests {
			result := <-results
			if result < 2 {
				wins++
			} else {
				i := result - 2
				got, _ := s.GetSession(t.Context(), sessions[i].RegistrationID)
				if got.Name != requests[i].Name || got.Revision != sessions[i].Revision {
					t.Fatal("loser changed")
				}
			}
		}
		if wins != 1 {
			t.Fatal("name contention did not have exactly one winner")
		}
		listed, _ := s.ListSessions(t.Context())
		if len(listed) != 2 {
			t.Fatal("contention lost registration")
		}
	}
}

func TestMisuseD2HeldQueueAcrossRenameStopSuspendAndHandover(t *testing.T) {
	for _, action := range []string{"rename", "stop", "suspend", "handover"} {
		t.Run(action, func(t *testing.T) {
			s, now, req, session := expiryQueueFixture(t)
			s.SetAdmission(&ownerTestAdmission{owner: "owner"})
			// Existing test registration predates the admission callback.
			s.byRegistration[session.RegistrationID].record.OwnerID = "owner"
			expiryQueueSend(t, s, "accepted-before-crash")
			*now = now.Add(remoteLeaseDuration + time.Second)
			s.sweep()
			if action == "stop" {
				if err := s.StopSession(t.Context(), session.RegistrationID); err != nil {
					t.Fatal(err)
				}
			}
			if action == "suspend" {
				if err := s.SetSuspended(t.Context(), session.RegistrationID, true); err != nil {
					t.Fatal(err)
				}
			}
			if action == "stop" || action == "suspend" {
				current, _ := s.GetSession(t.Context(), session.RegistrationID)
				if current.PendingMessages != 0 {
					t.Fatal("terminal transition retained queue")
				}
				return
			}
			if action == "rename" {
				// Ordinary model rename requires a live lease; proved direct recovery can
				// rename the same missing registration without changing queue ownership.
				proof, err := s.RenameDirectRegistration(t.Context(), session.RegistrationID, req.Secret, session.Revision, "renamed")
				if err != nil {
					t.Fatal("proved rename failed")
				}
				session.Revision = proof.Revision
			} else {
				next := req
				next.SessionID = sessionUUID(3)
				next.RequestID = "clear-handover"
				next.ExpectedRevision = session.Revision
				ctx := access.WithGrant(t.Context(), access.Grant{OwnerID: "owner", Scopes: []string{access.SessionRegister}})
				proof, err := s.RebindConversation(ctx, next, session.RegistrationID)
				if err != nil {
					t.Fatal("handover failed")
				}
				session.Revision = proof.Revision
				req = next
			}
			resumed := expiryQueueResume(t, s, req, session)
			expiryQueueReceive(t, s, resumed.RegistrationID, "resumed-lease", "accepted-before-crash")
			expiryQueueEmpty(t, s, resumed.RegistrationID, "resumed-lease")
		})
	}
}

func TestMisuseD3OldSelectorsAndReusedNames(t *testing.T) {
	s, _, req, session := expiryQueueFixture(t)
	s.SetAdmission(&ownerTestAdmission{owner: "owner"})
	s.byRegistration[session.RegistrationID].record.OwnerID = "owner"
	if _, err := s.RenameSession(t.Context(), RenameRequest{RegistrationID: session.RegistrationID, ChannelID: req.ChannelID, Name: "new-name"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendMessage(t.Context(), Message{From: "new-name", To: "recipient", Body: "stale name"}); err != ErrSessionNotFound {
		t.Fatal("stale unused name routed")
	}
	next := req
	next.SessionID = sessionUUID(3)
	next.RequestID = "handover"
	next.ExpectedRevision = 2
	if _, err := s.RebindConversation(access.WithGrant(t.Context(), access.Grant{OwnerID: "owner", Scopes: []string{access.SessionRegister}}), next, session.RegistrationID); err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{req.SessionID, SelectorSession + req.SessionID} {
		if _, err := s.GetSession(t.Context(), selector); err != ErrSessionNotFound {
			t.Fatal("old conversation ID still resolves")
		}
	}
	// A previously unseen reusable name still resolves its current holder.
	// Exchanged names are fenced separately by TestStaleNameReceivedPeerRefusesReplacement.
	other, err := s.RegisterSession(t.Context(), durableRegister(2, "recipient", "client-other", durableSecretB, "lease-other", "other"))
	if err != nil || other.Code != "" {
		t.Fatal("name reuse fixture failed")
	}
	resolved, err := s.GetSession(t.Context(), "recipient")
	if err != nil || resolved.RegistrationID != other.Session.RegistrationID || resolved.RegistrationID == session.RegistrationID {
		t.Fatal("name did not resolve current holder")
	}
}

func TestMisuseD8ConcurrentSendersAndRecipientResumeKeepCapAndFIFO(t *testing.T) {
	for iteration := 0; iteration < 10; iteration++ {
		s, _, req, session := expiryQueueFixture(t)
		for i := 0; i < 8; i++ {
			r := durableRegister(i+2, fmt.Sprintf("sender-%d", i), fmt.Sprintf("client-%d", i), durableSecretB, fmt.Sprintf("sender-lease-%d", i), fmt.Sprintf("sender-request-%d", i))
			if out, err := s.RegisterSession(t.Context(), r); err != nil || out.Code != "" {
				t.Fatal("sender fixture failed")
			}
		}
		var group sync.WaitGroup
		type acknowledgement struct {
			body     string
			sequence uint64
		}
		accepted := make(chan acknowledgement, 64)
		rejected := make(chan error, 64)
		start := make(chan struct{})
		for i := 0; i < 64; i++ {
			group.Add(1)
			go func(i int) {
				defer group.Done()
				<-start
				body := fmt.Sprintf("message-%d", i)
				out, err := s.SendMessage(t.Context(), Message{From: fmt.Sprintf("sender-%d", i%8), To: "recipient", Body: body})
				if err == nil && out.Accepted {
					accepted <- acknowledgement{body: body, sequence: out.DeliverySequence}
				} else {
					rejected <- err
				}
			}(i)
		}
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			out, err := s.ResumeSession(t.Context(), resumeFrom(req, session.RegistrationID, "new-lease", "resume", session.Revision))
			if err != nil || out.Code != "" {
				rejected <- fmt.Errorf("resume failed")
			}
		}()
		close(start)
		group.Wait()
		close(accepted)
		close(rejected)
		want := map[string]bool{}
		ordered := []acknowledgement{}
		for ack := range accepted {
			if want[ack.body] {
				t.Fatal("duplicate acceptance")
			}
			want[ack.body] = true
			ordered = append(ordered, ack)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].sequence < ordered[j].sequence })
		if len(want) != 32 {
			t.Fatalf("accepted count=%d want=32", len(want))
		}
		count := 0
		for err := range rejected {
			if err != ErrQueueFull {
				t.Fatal("unexpected send/resume failure")
			}
			count++
		}
		if count != 32 {
			t.Fatal("overflow count incorrect")
		}
		// The acknowledged delivery sequence defines the public FIFO order.
		for i, ack := range ordered {
			if ack.sequence != uint64(i+1) {
				t.Fatal("acceptance sequence has gaps or duplicates")
			}
			expiryQueueReceive(t, s, session.RegistrationID, "new-lease", ack.body)
			delete(want, ack.body)
		}
		if len(want) != 0 {
			t.Fatal("accepted message lost or duplicated")
		}
		expiryQueueEmpty(t, s, session.RegistrationID, "new-lease")
	}
}
