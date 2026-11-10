package broker

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func announcementCall(t *testing.T, s *RemoteService, name, body string) (SendResult, error) {
	t.Helper()
	sender, ok := any(s).(interface {
		SendAnnouncement(context.Context, string, string) (SendResult, error)
	})
	if !ok {
		t.Fatal("server announcement queue entry point missing")
	}
	return sender.SendAnnouncement(t.Context(), name, body)
}
func TestAnnouncementUsesExistingIngressAndQueueLimits(t *testing.T) {
	s := NewRemoteService()
	defer s.Close()
	a := remoteIdentity(1)
	b := remoteIdentity(2)
	for _, r := range []AttachRequest{a, b} {
		if _, e := s.AttachSession(t.Context(), r); e != nil {
			t.Fatal(e)
		}
	}
	for _, body := range []string{"", "  ", strings.Repeat("x", 12000)} {
		if _, e := announcementCall(t, s, b.Name, body); e == nil {
			t.Fatalf("invalid body accepted: length %d", len(body))
		}
	}
	for i := 0; i < 32; i++ {
		if v, e := announcementCall(t, s, a.Name, "notice"); e != nil || !v.Accepted {
			t.Fatal(v, e)
		}
	}
	if _, e := announcementCall(t, s, a.Name, "full"); !errors.Is(e, ErrQueueFull) {
		t.Fatal("queue cap", e)
	}
	if v, e := announcementCall(t, s, b.Name, "other"); e != nil || !v.Accepted {
		t.Fatal(v, e)
	}
	m, e := s.WaitForIngress(t.Context(), b.Name, b.ChannelID)
	if e != nil || m.From != "broker-admin" || m.To != b.Name || m.Body != "other" {
		t.Fatal(m, e)
	}
	if _, e = s.SendMessage(t.Context(), Message{From: b.Name, To: "broker-admin", Body: "reply"}); !errors.Is(e, ErrSessionNotFound) {
		t.Fatal("reserved sender reply did not clearly fail", e)
	}
	s.mu.Lock()
	s.sessions[b.Name].session.AutomaticDeliveryReady = false
	s.mu.Unlock()
	if _, e = announcementCall(t, s, b.Name, "not ready"); e == nil {
		t.Fatal("non-ready target accepted")
	}
}
func TestAnnouncementSenderNameCannotBeRegisteredOrRenamed(t *testing.T) {
	s := NewRemoteService()
	defer s.Close()
	a := remoteIdentity(1)
	a.Name = "broker-admin"
	if _, e := s.AttachSession(t.Context(), a); e == nil {
		t.Error("legacy reserved name accepted")
	}
	r := durableRegister(2, "broker-admin", "pc-test-01", durableSecretA, "lease-2", "request-2")
	if out, e := s.RegisterSession(t.Context(), r); e != nil || out.Code != CodeInvalid {
		t.Errorf("durable reserved name: %+v %v", out, e)
	}
	r.Name = "normal"
	out, e := s.RegisterSession(t.Context(), r)
	if e != nil || out.Code != "" {
		t.Fatal(out, e)
	}
	if _, e = s.RenameSession(t.Context(), RenameRequest{Name: "broker-admin", RegistrationID: out.Session.RegistrationID, ChannelID: r.ChannelID}); e == nil {
		t.Error("reserved rename accepted")
	}
}

func TestAnnouncementReservedSenderCannotBeSpoofedByOldRegistrationSelector(t *testing.T) {
	s := NewRemoteService()
	defer s.Close()
	a, b := remoteIdentity(1), remoteIdentity(2)
	for _, r := range []AttachRequest{a, b} {
		if _, e := s.AttachSession(t.Context(), r); e != nil {
			t.Fatal(e)
		}
	}
	// A pre-upgrade durable/legacy name can still be addressed by identity.
	s.mu.Lock()
	entry := s.sessions[a.Name]
	delete(s.sessions, a.Name)
	entry.session.Name = "broker-admin"
	s.sessions["broker-admin"] = entry
	s.mu.Unlock()
	if _, e := s.SendMessage(t.Context(), Message{From: SelectorSession + a.SessionID, To: b.Name, Body: "forged notice"}); e == nil {
		t.Fatal("old reserved registration selector spoofed administrator")
	}
}

func TestAnnouncementMixedCaseNameCannotRegisterRenameOrSend(t *testing.T) {
	for _, name := range []string{"Broker-Admin", "BROKER-ADMIN"} {
		for _, operation := range []string{"legacy", "durable", "rename", "sender"} {
			t.Run(name+"/"+operation, func(t *testing.T) {
				s := NewRemoteService()
				defer s.Close()
				switch operation {
				case "legacy":
					a := remoteIdentity(1)
					a.Name = name
					if _, err := s.AttachSession(t.Context(), a); err == nil {
						t.Error("mixed-case legacy registration accepted")
					}
				case "durable":
					r := durableRegister(2, name, "pc-test-01", durableSecretA, "lease-2", "request-2")
					if out, err := s.RegisterSession(t.Context(), r); err != nil || out.Code != CodeInvalid {
						t.Errorf("mixed-case durable registration: code=%s error=%v", out.Code, err)
					}
				case "rename":
					r := durableRegister(2, "normal", "pc-test-01", durableSecretA, "lease-2", "request-2")
					out, err := s.RegisterSession(t.Context(), r)
					if err != nil || out.Code != "" {
						t.Fatal(out.Code, err)
					}
					if _, err = s.RenameSession(t.Context(), RenameRequest{Name: name, RegistrationID: out.Session.RegistrationID, ChannelID: r.ChannelID}); err == nil {
						t.Error("mixed-case rename accepted")
					}
				case "sender":
					a, b := remoteIdentity(1), remoteIdentity(3)
					for _, r := range []AttachRequest{a, b} {
						if _, err := s.AttachSession(t.Context(), r); err != nil {
							t.Fatal(err)
						}
					}
					// Simulate a pre-upgrade name without relying on the registration under test.
					s.mu.Lock()
					entry := s.sessions[a.Name]
					delete(s.sessions, a.Name)
					entry.session.Name = name
					s.sessions[name] = entry
					s.mu.Unlock()
					for _, from := range []string{name, SelectorSession + a.SessionID} {
						if _, err := s.SendMessage(t.Context(), Message{From: from, To: b.Name, Body: "forged notice"}); err == nil {
							t.Errorf("mixed-case sender accepted via %s", from)
						}
					}
					for _, to := range []string{name, SelectorName + name, SelectorSession + a.SessionID} {
						if _, err := s.SendMessage(t.Context(), Message{From: b.Name, To: to, Body: "reply"}); !errors.Is(err, ErrSessionNotFound) {
							t.Errorf("reply to mixed-case reserved selector %s: %v", to, err)
						}
					}
				}
			})
		}
	}
}
