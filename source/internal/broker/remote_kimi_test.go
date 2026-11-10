package broker

import (
	"testing"
	"time"
)

func TestKimiRemoteDurableRestartAndResume(t *testing.T) {
	for _, sqlite := range []bool{false, true} {
		name := "json"
		if sqlite {
			name = "sqlite"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			open := func() *RemoteService {
				s, err := newRemoteServiceWithStore(root, false, sqlite)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = s.Close() })
				return s
			}
			s := open()
			req := durableRegister(1, "kimi-worker", "client-kimi-0001", durableSecretA, "kimi-lease-1", "kimi-register")
			req.Harness = "kimi"
			first, err := s.RegisterSession(t.Context(), req)
			if err != nil || first.Code != "" || first.Session.Harness != "kimi" || !first.Session.AutomaticDeliveryReady {
				t.Fatalf("Kimi register: code=%s err=%v", first.Code, err)
			}
			now := s.now().UTC()
			activity := activityRenewJSON(t, s, first.Session.RegistrationID, req.ChannelID, map[string]any{
				"state": "unknown", "reason": "unsupported", "source": "", "sequence": 1, "observedAt": now.Format(time.RFC3339Nano),
			}).Session.Activity
			if activity == nil || activity.State != "unknown" || activity.Reason != "unsupported" || activity.Source != "" || activity.Sequence != 1 || activity.ReceivedAt.IsZero() {
				t.Fatal("Kimi unsupported activity was not accepted")
			}
			resumed, err := s.ResumeSession(t.Context(), resumeFrom(req, first.Session.RegistrationID, "kimi-lease-2", "kimi-resume", 1))
			if err != nil || resumed.Code != "" || resumed.Session.Revision != 2 || resumed.Session.Harness != "kimi" {
				t.Fatalf("Kimi resume: code=%s err=%v", resumed.Code, err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = open()
			restored, err := s.GetSession(t.Context(), "kimi-worker")
			if err != nil || restored.Harness != "kimi" || restored.RegistrationID != first.Session.RegistrationID || restored.Status != StatusMissing || restored.AutomaticDeliveryReady {
				t.Fatalf("Kimi restart did not restore missing registration: err=%v", err)
			}
			resumed, err = s.ResumeSession(t.Context(), resumeFrom(req, first.Session.RegistrationID, "kimi-lease-3", "kimi-restart-resume", 2))
			if err != nil || resumed.Code != "" || resumed.Session.Status != StatusAlive || !resumed.Session.AutomaticDeliveryReady {
				t.Fatalf("Kimi resume after restart: code=%s err=%v", resumed.Code, err)
			}
			sent, err := s.SendMessage(t.Context(), Message{From: req.Name, To: req.Name, Body: "kimi round trip"})
			if err != nil || !sent.Accepted {
				t.Fatal("Kimi message rejected", err)
			}
			got, err := s.WaitForIngress(t.Context(), req.Name, "kimi-lease-3")
			if err != nil || got.Body != "kimi round trip" {
				t.Fatal("Kimi ingress failed", err)
			}
		})
	}
}

func TestKimiRemoteUnknownHarnessStillRejected(t *testing.T) {
	s, _ := clockedService(t, "")
	req := durableRegister(1, "worker", "client-kimi-0001", durableSecretA, "lease-1", "register")
	req.Harness = "other"
	if out, err := s.RegisterSession(t.Context(), req); err != nil || out.Code != CodeInvalid {
		t.Fatalf("unknown harness register: code=%s err=%v", out.Code, err)
	}
	req.Harness = "claude"
	first, err := s.RegisterSession(t.Context(), req)
	if err != nil || first.Code != "" {
		t.Fatal("fixture registration failed")
	}
	resume := resumeFrom(req, first.Session.RegistrationID, "lease-2", "resume", 1)
	resume.Harness = "other"
	if out, err := s.ResumeSession(t.Context(), resume); err != nil || out.Code != CodeInvalid {
		t.Fatalf("unknown harness resume: code=%s err=%v", out.Code, err)
	}
	s.mu.Lock()
	record := *s.byRegistration[first.Session.RegistrationID].record
	s.mu.Unlock()
	record.Harness = "other"
	if record.valid() {
		t.Fatal("unknown stored harness accepted")
	}
}

func TestKimiLegacyRemoteAttach(t *testing.T) {
	for _, harness := range []string{"kimi", "other"} {
		t.Run(harness, func(t *testing.T) {
			s := NewRemoteService()
			defer s.Close()
			req := remoteIdentity(1)
			req.Harness = harness
			got, err := s.AttachSession(t.Context(), req)
			if harness == "other" {
				if err == nil {
					t.Fatal("unknown remote harness accepted")
				}
				return
			}
			if err != nil || got.Harness != "kimi" || !got.AutomaticDeliveryReady {
				t.Fatal("Kimi remote attach failed", err)
			}
		})
	}
}

func TestKimiRecipientSendReportsTurnBoundaryDelivery(t *testing.T) {
	for _, harness := range []string{"kimi", "claude", "codex"} {
		t.Run(harness, func(t *testing.T) {
			s, _ := clockedService(t, "")
			sender := durableRegister(1, "kimi-sender", "client-kimi-0001", durableSecretA, "sender-lease", "sender-register")
			sender.Harness = "kimi"
			recipient := durableRegister(2, "recipient", "client-recipient", durableSecretB, "recipient-lease", "recipient-register")
			recipient.Harness = harness
			for _, req := range []RegisterRequest{sender, recipient} {
				if out, err := s.RegisterSession(t.Context(), req); err != nil || out.Code != "" {
					t.Fatal("registration failed", out.Code, err)
				}
			}
			sent, err := s.SendMessage(t.Context(), Message{From: sender.Name, To: recipient.Name, Body: "turn boundary"})
			if err != nil || !sent.OK || !sent.Accepted || sent.MeteredWake || sent.DeliverySequence != 1 {
				t.Fatal("queue acceptance changed", err)
			}
			if harness == "kimi" {
				if sent.WakeStatus != WakeDegraded || sent.WarningMessage != "queued for the next eligible UserPromptSubmit or Stop hook; idle, failed or continued turns may wait until the next user prompt" {
					t.Fatalf("Kimi delivery hint: wake=%s warning=%q", sent.WakeStatus, sent.WarningMessage)
				}
			} else if sent.WakeStatus != WakeNotNeeded || sent.Warning != "" || sent.WarningMessage != "" {
				t.Fatal("Kimi sender changed non-Kimi recipient result")
			}
			queued, err := s.WaitForIngress(t.Context(), recipient.Name, recipient.ChannelID)
			if err != nil || queued.Body != "turn boundary" || queued.To != recipient.Name {
				t.Fatal("delivery hint changed queued message", err)
			}
		})
	}
}
