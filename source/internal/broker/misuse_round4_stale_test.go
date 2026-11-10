package broker

import (
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"testing"
)

func TestMisuseK1BatchConflictingNamesRequireExplicitConfirmation(t *testing.T) {
	s, r, sender, replacement := staleNameFixture(t)
	e := s.byRegistration[sender.RegistrationID]
	old := e.observedPeers[replacement.Name].registrationID
	e.queue = append(e.queue, Message{Batch: &protocol.MessageBatch{Messages: []protocol.DeliveredMessage{{From: "recipient", FromRegistrationID: old, Body: "old"}, {From: "recipient", FromRegistrationID: replacement.RegistrationID, Body: "new"}}}})
	if _, err := s.WaitForIngress(t.Context(), r.Name, r.ChannelID); err != nil {
		t.Fatal(err)
	}
	out, err := s.SendMessage(t.Context(), Message{From: sender.Name, To: "recipient", Body: "ambiguous"})
	if err != nil || out.Code != protocol.CodeRecipientChanged || out.Accepted || len(s.byRegistration[replacement.RegistrationID].queue) != 0 {
		t.Fatal("batch cleared ambiguity", err)
	}
	if _, err := s.SendMessage(t.Context(), Message{From: sender.Name, To: "recipient", Body: "explicit", ExpectRegistrationID: &replacement.RegistrationID}); err != nil {
		t.Fatal(err)
	}
	if e.observedPeers["recipient"].ambiguous {
		t.Fatal("explicit matching ID did not confirm")
	}
}
func TestMisuseK1SenderRemovalAndHotPeerBudget(t *testing.T) {
	s, _, sender, replacement := staleNameFixture(t)
	e := s.byRegistration[sender.RegistrationID]
	for i := 0; i < 600; i++ {
		s.observePeerLocked(t.Context(), e, fmt.Sprintf("cold-%d", i), fmt.Sprintf("opaque-%d", i), false)
		if i%8 == 0 {
			s.observePeerLocked(t.Context(), e, "recipient", e.observedPeers["recipient"].registrationID, false)
		}
	}
	if len(e.observedPeers) != 256 || e.observedPeers["recipient"].registrationID == "" {
		t.Fatal("active peer evicted")
	}
	if out, _ := s.SendMessage(t.Context(), Message{From: sender.Name, To: replacement.Name, Body: "still fenced"}); out.Accepted || out.Code != protocol.CodeRecipientChanged {
		t.Fatal("budget erased active pin")
	}
	if err := s.removeLocked(e, CodeDetached); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.RegisterSession(t.Context(), durableRegister(8, "sender", "fresh-sender-client", durableSecretB, "fresh-sender-lease", "fresh-sender-request"))
	if err != nil || fresh.Code != "" {
		t.Fatal("fresh sender setup")
	}
	if out, err := s.SendMessage(t.Context(), Message{From: fresh.Session.Name, To: replacement.Name, Body: "fresh registration"}); err != nil || !out.Accepted {
		t.Fatal("removed sender's history leaked into new holder")
	}
}
func TestMisuseK1RenameChainDoesNotConfuseRegistration(t *testing.T) {
	s, _ := clockedService(t, "")
	a := durableRegister(1, "A", "a-client", durableSecretA, "a-lease", "a-request")
	ar, err := s.RegisterSession(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	observer, err := s.RegisterSession(t.Context(), durableRegister(2, "S", "s-client", durableSecretB, "s-lease", "s-request"))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := s.SendMessage(t.Context(), Message{From: observer.Session.Name, To: "A", Body: "learn"}); err != nil || !out.Accepted {
		t.Fatal("learn")
	}
	if _, err := s.RenameSession(t.Context(), RenameRequest{RegistrationID: ar.Session.RegistrationID, ChannelID: a.ChannelID, Name: "B"}); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.RegisterSession(t.Context(), durableRegister(3, "C", "c-client", durableSecretB, "c-lease", "c-request"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameSession(t.Context(), RenameRequest{RegistrationID: replacement.Session.RegistrationID, ChannelID: "c-lease", Name: "A"}); err != nil {
		t.Fatal(err)
	}
	if out, _ := s.SendMessage(t.Context(), Message{From: observer.Session.Name, To: "A", Body: "old A"}); out.Code != protocol.CodeRecipientChanged || out.Accepted {
		t.Fatal("rename chain misdelivered")
	}
	if out, err := s.SendMessage(t.Context(), Message{From: observer.Session.Name, To: "B", Body: "same registration", ExpectRegistrationID: &ar.Session.RegistrationID}); err != nil || !out.Accepted {
		t.Fatal("same registration's new name rejected")
	}
	if len(s.byRegistration[replacement.Session.RegistrationID].queue) != 0 {
		t.Fatal("replacement queue nonempty")
	}
}

func TestMisuseK1ReuseRacesWithSend(t *testing.T) {
	for i := 0; i < 40; i++ {
		s, _ := clockedService(t, "")
		oldReq := durableRegister(1, "peer", "old-client", durableSecretA, "old-lease", "old-request")
		old, err := s.RegisterSession(t.Context(), oldReq)
		if err != nil {
			t.Fatal(err)
		}
		sender, err := s.RegisterSession(t.Context(), durableRegister(2, "sender", "sender-client", durableSecretB, "sender-lease", "sender-request"))
		if err != nil {
			t.Fatal(err)
		}
		if out, _ := s.SendMessage(t.Context(), Message{From: sender.Session.Name, To: "peer", Body: "learn"}); !out.Accepted {
			t.Fatal("learn")
		}
		start := make(chan struct{})
		done := make(chan struct{})
		var replacement RegisterResult
		var replaceErr error
		go func() {
			defer close(done)
			<-start
			s.mu.Lock()
			replaceErr = s.removeLocked(s.byRegistration[old.Session.RegistrationID], CodeDetached)
			s.mu.Unlock()
			if replaceErr == nil {
				replacement, replaceErr = s.RegisterSession(t.Context(), durableRegister(3, "peer", "new-client", durableSecretB, "new-lease", "new-request"))
			}
		}()
		close(start)
		_, _ = s.SendMessage(t.Context(), Message{From: sender.Session.Name, To: "peer", Body: "racing reply"})
		<-done
		if replaceErr != nil || replacement.Code != "" {
			t.Fatal("replacement failed")
		}
		s.mu.Lock()
		queued := len(s.byRegistration[replacement.Session.RegistrationID].queue)
		s.mu.Unlock()
		if queued != 0 {
			t.Fatal("concurrent reuse delivered stale reply")
		}
	}
}

func TestMisuseK1AdminAnnouncementDoesNotClearFence(t *testing.T) {
	s, r, sender, replacement := staleNameFixture(t)
	if out, err := s.SendAnnouncement(t.Context(), sender.Name, "admin notice"); err != nil || !out.Accepted {
		t.Fatal("announcement enqueue", err)
	}
	if message, err := s.WaitForIngress(t.Context(), sender.Name, r.ChannelID); err != nil || message.From != protocol.AdminSender {
		t.Fatal("announcement delivery", err)
	}
	if _, ok := s.byRegistration[sender.RegistrationID].observedPeers[protocol.AdminSender]; ok {
		t.Fatal("admin recorded as peer")
	}
	if out, err := s.SendMessage(t.Context(), Message{From: sender.Name, To: replacement.Name, Body: "still stale"}); err != nil || out.Accepted || out.Code != protocol.CodeRecipientChanged {
		t.Fatal("announcement cleared unrelated fence")
	}
}
