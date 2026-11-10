package broker

import (
	"context"
	"testing"
	"time"
)

func TestDurablePendingIngressRegistersWithoutAdvertisingDelivery(t *testing.T) {
	ctx := context.Background()
	s, now := clockedService(t, "")
	r := durableRegister(1, "pending", "client-pending-001", durableSecretA, "lease-pending", "register-pending")
	r.IngressPending = true
	registered, err := s.RegisterSession(ctx, r)
	if err != nil || registered.Code != "" || registered.Session.RegistrationID == "" || registered.Session.Status != StatusAlive || registered.Session.AutomaticDeliveryReady {
		t.Fatalf("pending registration must be alive but not delivery-ready: %+v %v", registered, err)
	}
	if result, err := s.SendMessage(ctx, Message{From: r.Name, To: r.Name, Body: "not activated"}); err == nil || result.Accepted {
		t.Fatal("message accepted before activation")
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if _, err := s.WaitForIngress(waitCtx, r.Name, r.ChannelID); err == nil || waitCtx.Err() != nil {
		t.Fatal("pending receiver must reject immediately before taking any queued message")
	}
	*now = now.Add(20 * time.Second)
	renew := RenewRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: r.ChannelID, IngressPending: true}
	kept, err := s.RenewLease(ctx, renew)
	if err != nil || kept.Code != "" || kept.Session.Status != StatusAlive || kept.Session.AutomaticDeliveryReady {
		t.Fatalf("heartbeat lost pending status: %+v %v", kept, err)
	}
	wrong := renew
	wrong.ChannelID, wrong.IngressPending = "wrong-lease", false
	if rejected, _ := s.RenewLease(ctx, wrong); rejected.Code != CodeInvalid {
		t.Fatal("foreign lease activated registration")
	}
	renew.IngressPending = false
	ready, err := s.RenewLease(ctx, renew)
	if err != nil || ready.Code != "" || !ready.Session.AutomaticDeliveryReady || ready.Session.Revision != registered.Session.Revision {
		t.Fatalf("activation must preserve registration revision: %+v %v", ready, err)
	}
	delayed := renew
	delayed.IngressPending = true
	if stale, _ := s.RenewLease(ctx, delayed); !stale.Session.AutomaticDeliveryReady {
		t.Fatal("delayed pending heartbeat reversed activation")
	}
	// A lost registration response must not reset a later activation.
	if replay, err := s.RegisterSession(ctx, r); err != nil || replay.Code != "" || !replay.Session.AutomaticDeliveryReady {
		t.Fatalf("registration replay reset channel readiness: %+v %v", replay, err)
	}
	if _, err := s.SendMessage(ctx, Message{From: r.Name, To: r.Name, Body: "activated"}); err != nil {
		t.Fatal(err)
	}
	if msg, err := s.WaitForIngress(ctx, r.Name, r.ChannelID); err != nil || msg.Body != "activated" {
		t.Fatalf("activated delivery failed: %+v %v", msg, err)
	}
	fresh := r
	fresh.RequestID, fresh.ExpectedRevision = "same-lease-register", ready.Session.Revision
	keptRegistration, err := s.RegisterSession(ctx, fresh)
	if err != nil || keptRegistration.Code != "" || !keptRegistration.Session.AutomaticDeliveryReady {
		t.Fatalf("same-lease registration reversed activation: %+v %v", keptRegistration, err)
	}
	resume := resumeFrom(r, registered.Session.RegistrationID, "replacement", "resume-pending", keptRegistration.Session.Revision)
	resume.IngressPending = true
	next, err := s.ResumeSession(ctx, resume)
	if err != nil || next.Code != "" || next.Session.AutomaticDeliveryReady || next.Session.Generation <= ready.Session.Generation {
		t.Fatalf("replacement channel must wait for its own activation: %+v %v", next, err)
	}
	if old, _ := s.RenewLease(ctx, renew); old.Code != CodeInvalid {
		t.Fatal("old receiver activated replacement")
	}
}
