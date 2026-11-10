package broker

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"testing"
	"time"
)

func TestLegacyRecoverDurableFreshLeaseRetains0171Behavior(t *testing.T) {
	s, _ := clockedService(t, "")
	r := durableRegister(901, "legacy-reconnect", "legacy-client", durableSecretA, "legacy-a", "register-a")
	r.Holder = nil // A 0.17.1 client has neither holder nor challenge fields.
	first, err := s.RegisterSession(t.Context(), r)
	if err != nil || first.Code != "" {
		t.Fatal("legacy fixture registration")
	}
	if _, err := s.SendMessage(t.Context(), Message{From: r.Name, To: r.Name, Body: "queued-before-reconnect"}); err != nil {
		t.Fatal("legacy queue fixture")
	}
	// 0.17.1 recoverDurable sends a fresh lease even in the same live process.
	req := resumeFrom(r, first.Session.RegistrationID, "legacy-b", "recover-b", first.Session.Revision)
	next, err := s.ResumeSession(t.Context(), req)
	if err != nil || next.Code != "" {
		t.Fatal("ordinary legacy fresh-lease reconnect was blocked")
	}
	if next.Session.RegistrationID != first.Session.RegistrationID || next.Session.Name != first.Session.Name {
		t.Fatal("legacy reconnect replaced registration")
	}
	replay, _ := s.ResumeSession(t.Context(), req)
	if replay.Code != "" || replay.Session.Revision != next.Session.Revision {
		t.Fatal("legacy exact retry lost idempotency")
	}
	old, _ := s.RenewLease(t.Context(), RenewRequest{RegistrationID: first.Session.RegistrationID, ChannelID: r.ChannelID})
	if old.Code != "invalid" {
		t.Fatal("legacy old lease was not fenced")
	}
	got, err := s.WaitForIngress(t.Context(), r.Name, req.ChannelID)
	if err != nil || got.Body != "queued-before-reconnect" {
		t.Fatal("legacy reconnect lost queue")
	}
	wrong := req
	wrong.RequestID = "wrong-proof"
	wrong.ChannelID = "legacy-c"
	wrong.ExpectedRevision = next.Session.Revision
	wrong.Secret = durableSecretB
	denied, _ := s.ResumeSession(t.Context(), wrong)
	if denied.Code != CodeOwned {
		t.Fatal("legacy exemption bypassed authentication")
	}
	stale := req
	stale.RequestID = "stale-revision"
	stale.ChannelID = "legacy-c"
	denied, _ = s.ResumeSession(t.Context(), stale)
	if denied.Code != CodeRevisionMismatch {
		t.Fatal("legacy exemption bypassed revision")
	}
}

func TestMixedHolderLegacyCompatibilityDoesNotWeakenGuard(t *testing.T) {
	for _, modernOwner := range []bool{false, true} {
		t.Run(map[bool]string{false: "modern-requester-legacy-holder", true: "legacy-requester-modern-holder"}[modernOwner], func(t *testing.T) {
			s, now := clockedService(t, "")
			r := durableRegister(902, "mixed-holder", "mixed-client", durableSecretA, "lease-a", "register-a")
			r.Holder = nil
			if modernOwner {
				r.Holder = syntheticReceiverHolder(901, "a")
			}
			first, _ := s.RegisterSession(t.Context(), r)
			req := resumeFrom(r, first.Session.RegistrationID, "lease-b", "resume-b", first.Session.Revision)
			req.Holder = nil
			if !modernOwner {
				req.Holder = syntheticReceiverHolder(902, "a")
			}
			for i := 0; i < 4; i++ {
				blocked, _ := s.ResumeSession(t.Context(), req)
				if blocked.Code != protocol.CodeRegistrationInUse {
					t.Fatal("mixed live holder replaced")
				}
				*now = now.Add(20 * time.Second)
				renewed, _ := s.RenewLease(t.Context(), RenewRequest{RegistrationID: first.Session.RegistrationID, ChannelID: r.ChannelID})
				if renewed.Code != "" {
					t.Fatal("mixed owner lost lease")
				}
			}
			*now = now.Add(31 * time.Second)
			allowed, _ := s.ResumeSession(t.Context(), req)
			if allowed.Code != "" {
				t.Fatal("mixed takeover blocked after actual expiry")
			}
		})
	}
}

func TestInvalidOrPartialHolderIsNotLegacyAbsence(t *testing.T) {
	s, _ := clockedService(t, "")
	r := durableRegister(903, "invalid-holder", "invalid-client", durableSecretA, "lease-a", "register-a")
	r.Holder = nil
	first, _ := s.RegisterSession(t.Context(), r)
	for _, holder := range []*protocol.ReceiverHolder{{}, {PID: 10}, {DeviceID: syntheticReceiverHolder(10, "a").DeviceID}, {PID: 10, DeviceID: "invalid", Incarnation: "invalid"}} {
		req := resumeFrom(r, first.Session.RegistrationID, "lease-b", "resume-b", first.Session.Revision)
		req.Holder = holder
		result, _ := s.ResumeSession(t.Context(), req)
		if result.Code != CodeInvalid {
			t.Fatal("invalid or partial holder treated as absent")
		}
		reg := r
		reg.Holder = holder
		reg.RequestID = "register-b"
		reg.ChannelID = "lease-b"
		reg.ExpectedRevision = first.Session.Revision
		result, _ = s.RegisterSession(t.Context(), reg)
		if result.Code != CodeInvalid {
			t.Fatal("invalid register holder treated as absent")
		}
	}
	current, _ := s.RenewLease(t.Context(), RenewRequest{RegistrationID: first.Session.RegistrationID, ChannelID: r.ChannelID})
	if current.Code != "" {
		t.Fatal("invalid holder changed original lease")
	}
}
