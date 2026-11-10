package broker

import (
	"errors"
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"testing"
)

func staleNameFixture(t *testing.T) (*RemoteService, RegisterRequest, Session, Session) {
	t.Helper()
	s, _ := clockedService(t, "")
	add := func(n int, name string) (RegisterRequest, Session) {
		r := durableRegister(n, name, name+"-client", durableSecretA, name+"-lease", name+"-request")
		out, err := s.RegisterSession(t.Context(), r)
		if err != nil || out.Code != "" {
			t.Fatal(err, out.Code)
		}
		return r, out.Session
	}
	r, sender := add(1, "sender")
	_, old := add(2, "recipient")
	if result, err := s.SendMessage(t.Context(), Message{From: old.Name, To: sender.Name, Body: "request"}); err != nil || !result.Accepted {
		t.Fatal(result, err)
	}
	if _, err := s.WaitForIngress(t.Context(), sender.Name, r.ChannelID); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	err := s.removeLocked(s.byRegistration[old.RegistrationID], CodeDetached)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	_, replacement := add(3, "recipient")
	return s, r, sender, replacement
}
func TestStaleNameReceivedPeerRefusesReplacement(t *testing.T) {
	s, _, sender, replacement := staleNameFixture(t)
	result, err := s.SendMessage(t.Context(), Message{From: sender.Name, To: "recipient", Body: "old reply"})
	if err != nil || result.Accepted || result.Code != protocol.CodeRecipientChanged {
		t.Fatalf("stale reply accepted: %+v %v", result, err)
	}
	if len(s.byRegistration[replacement.RegistrationID].queue) != 0 {
		t.Fatal("replacement received stale reply")
	}
}

func TestStaleNameAmbiguityRetryAndConfirmation(t *testing.T) {
	for _, ack := range []bool{false, true} {
		t.Run(fmt.Sprint(ack), func(t *testing.T) {
			s, r, sender, replacement := staleNameFixture(t)
			if ack {
				e := s.byRegistration[sender.RegistrationID]
				e.messageAck = true
			}
			if out, err := s.SendMessage(t.Context(), Message{From: replacement.Name, To: sender.Name, Body: "new owner"}); err != nil || !out.Accepted {
				t.Fatal(out, err)
			}
			if ack {
				_, err := s.WaitForIngressAck(t.Context(), protocol.IngressRequest{Recipient: sender.Name, ChannelID: r.ChannelID, RegistrationID: sender.RegistrationID, Epoch: sender.Epoch})
				if err != nil {
					t.Fatal(err)
				}
			} else if _, err := s.WaitForIngress(t.Context(), sender.Name, r.ChannelID); err != nil {
				t.Fatal(err)
			}
			refuse := func() {
				t.Helper()
				out, err := s.SendMessage(t.Context(), Message{From: sender.Name, To: "name:recipient", Body: "old reply"})
				if err != nil || out.Code != protocol.CodeRecipientChanged || out.Accepted || out.Message != staleNameMessage("name:recipient") {
					t.Fatal(out, err)
				}
			}
			refuse()
			refuse()
			if _, err := s.ListSessions(t.Context()); err != nil {
				t.Fatal(err)
			}
			refuse()
			if _, err := s.GetSession(t.Context(), "recipient"); err != nil {
				t.Fatal(err)
			}
			refuse()
			ctx := access.WithGrant(t.Context(), access.Grant{Scopes: []string{access.SessionRead}, Check: func(identity string) error {
				if identity == bindingForRename(s.byRegistration[sender.RegistrationID]) {
					return nil
				}
				return errors.New("not bound")
			}})
			if _, err := s.GetSessionObserved(ctx, "recipient"); err != nil {
				t.Fatal(err)
			}
			out, err := s.SendMessage(t.Context(), Message{From: sender.Name, To: "recipient", Body: "intentional reply"})
			if err != nil || !out.Accepted {
				t.Fatal(out, err)
			}
		})
	}
}
func TestStaleNameExplicitAddressesAndScopes(t *testing.T) {
	for _, mode := range []string{"new sender", "registration", "session", "expect", "no-read"} {
		t.Run(mode, func(t *testing.T) {
			s, _, sender, replacement := staleNameFixture(t)
			ctx := t.Context()
			m := Message{From: sender.Name, To: "recipient", Body: "intentional"}
			switch mode {
			case "new sender":
				out, err := s.RegisterSession(ctx, durableRegister(4, "new", "new-client", durableSecretB, "new-lease", "new-request"))
				if err != nil || out.Code != "" {
					t.Fatal(out, err)
				}
				m.From = out.Session.Name
			case "registration":
				m.To = SelectorRegistration + replacement.RegistrationID
			case "session":
				m.To = SelectorSession + replacement.ID
			case "expect":
				m.ExpectRegistrationID = &replacement.RegistrationID
			case "no-read":
				ctx = access.WithGrant(ctx, access.Grant{Scopes: []string{access.MessageSend}})
			}
			out, err := s.SendMessage(ctx, m)
			if err != nil || !out.Accepted {
				t.Fatal(out, err)
			}
			if mode == "expect" {
				m.ExpectRegistrationID = nil
				if out, err = s.SendMessage(ctx, m); err != nil || !out.Accepted {
					t.Fatal(out, err)
				}
			}
			if mode == "no-read" {
				peer := s.byRegistration[sender.RegistrationID].observedPeers["recipient"]
				if peer.registrationID == replacement.RegistrationID {
					t.Fatal("no-read changed observation")
				}
			}
		})
	}
}
func TestStaleNameStableRegistrationRenameAndRebind(t *testing.T) {
	s, _ := clockedService(t, "")
	r := durableRegister(1, "recipient", "peer-client", durableSecretA, "peer-lease", "peer-request")
	first, err := s.RegisterSession(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := s.RegisterSession(t.Context(), durableRegister(2, "sender", "sender-client", durableSecretB, "sender-lease", "sender-request"))
	if err != nil {
		t.Fatal(err)
	}
	send := func(to string) {
		t.Helper()
		o, e := s.SendMessage(t.Context(), Message{From: sender.Session.Name, To: to, Body: "stable"})
		if e != nil || !o.Accepted {
			t.Fatal(o, e)
		}
	}
	send("recipient")
	if _, err := s.RenameSession(t.Context(), RenameRequest{RegistrationID: first.Session.RegistrationID, ChannelID: r.ChannelID, Name: "renamed"}); err != nil {
		t.Fatal(err)
	}
	send("renamed")
	// A conversation rebind changes the UUID, not the registration's peer history.
	s.SetAdmission(&ownerTestAdmission{owner: "owner"})
	s.byRegistration[first.Session.RegistrationID].record.OwnerID = "owner"
	next := r
	next.SessionID = sessionUUID(5)
	next.RequestID = "clear"
	next.ExpectedRevision = 2
	_, err = s.RebindConversation(access.WithGrant(t.Context(), access.Grant{OwnerID: "owner", Scopes: []string{access.SessionRegister}}), next, first.Session.RegistrationID)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := s.ResumeSession(t.Context(), resumeFrom(next, first.Session.RegistrationID, "new-lease", "after-clear", 3))
	if err != nil || resumed.Code != "" {
		t.Fatal(resumed, err)
	}
	send("renamed")
}
func TestStaleNameCapRemovalBatchAndExclusions(t *testing.T) {
	s, r, sender, _ := staleNameFixture(t)
	e := s.byRegistration[sender.RegistrationID]
	for i := 0; i < 256; i++ {
		s.observePeerLocked(t.Context(), e, fmt.Sprintf("peer-%d", i), fmt.Sprintf("id-%d", i), false)
	}
	s.observePeerLocked(t.Context(), e, "peer-0", "id-0", false)
	s.observePeerLocked(t.Context(), e, "new-peer", "new-id", false)
	if len(e.observedPeers) != 256 {
		t.Fatal("cap")
	}
	if _, ok := e.observedPeers["peer-1"]; ok {
		t.Fatal("not oldest evicted")
	}
	s.observePeerLocked(t.Context(), e, "ignored", "", false)
	s.observePeerLocked(t.Context(), e, protocol.AdminSender, "admin-id", false)
	if _, ok := e.observedPeers[protocol.AdminSender]; ok {
		t.Fatal("admin sender recorded")
	}
	legacy := &remoteSession{}
	s.observePeerLocked(t.Context(), legacy, "peer", "id", false)
	if legacy.observedPeers != nil {
		t.Fatal("legacy sender recorded")
	}
	s.observePeerLocked(access.WithGrant(t.Context(), access.Grant{}), e, "no-read", "id", false)
	if _, ok := e.observedPeers["no-read"]; ok {
		t.Fatal("no-read recorded")
	}
	e.queue = append(e.queue, Message{Batch: &protocol.MessageBatch{Messages: []protocol.DeliveredMessage{{From: "batch-peer", FromRegistrationID: "batch-id", Body: "batch"}}}})
	if _, err := s.WaitForIngress(t.Context(), r.Name, r.ChannelID); err != nil {
		t.Fatal(err)
	}
	if e.observedPeers["batch-peer"].registrationID != "batch-id" {
		t.Fatal("batch not recorded")
	}
	if err := s.removeLocked(e, CodeDetached); err != nil {
		t.Fatal(err)
	}
	if e.observedPeers != nil {
		t.Fatal("removed registration retained peers")
	}
}
func TestStaleNameConcurrentReuseQueuesNothingToReplacement(t *testing.T) {
	for i := 0; i < 30; i++ {
		s, _, sender, replacement := staleNameFixture(t)
		s.mu.Lock()
		err := s.removeLocked(s.byRegistration[replacement.RegistrationID], CodeDetached)
		s.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		type registered struct {
			out RegisterResult
			err error
		}
		done := make(chan registered, 1)
		go func() {
			out, err := s.RegisterSession(t.Context(), durableRegister(4, "recipient", "race-client", durableSecretB, "race-lease", "race-request"))
			done <- registered{out, err}
		}()
		_, _ = s.SendMessage(t.Context(), Message{From: sender.Name, To: "recipient", Body: "race"})
		got := <-done
		if got.err != nil || got.out.Code != "" {
			t.Fatal(got.out, got.err)
		}
		if len(s.byRegistration[got.out.Session.RegistrationID].queue) != 0 {
			t.Fatal("replacement received raced stale reply")
		}
	}
}
