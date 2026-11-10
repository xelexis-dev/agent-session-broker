package broker

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func expiryQueueFixture(t *testing.T) (*RemoteService, *time.Time, RegisterRequest, Session) {
	t.Helper()
	s, now := clockedService(t, "")
	req := durableRegister(1, "recipient", "expiry-client", durableSecretA, "lease", "register")
	result, err := s.RegisterSession(t.Context(), req)
	if err != nil || result.Code != "" {
		t.Fatal("fixture registration failed")
	}
	return s, now, req, result.Session
}
func expiryQueueSend(t *testing.T, s *RemoteService, body string) {
	t.Helper()
	if sent, err := s.SendMessage(t.Context(), Message{From: "recipient", To: "recipient", Body: body}); err != nil || !sent.Accepted {
		t.Fatal("fixture message not accepted")
	}
}
func expiryQueueResume(t *testing.T, s *RemoteService, req RegisterRequest, session Session) Session {
	t.Helper()
	result, err := s.ResumeSession(t.Context(), resumeFrom(req, session.RegistrationID, "resumed-lease", "resume", session.Revision))
	if err != nil || result.Code != "" || result.Session.RegistrationID != session.RegistrationID {
		t.Fatal("same registration resume failed")
	}
	return result.Session
}
func expiryQueueReceive(t *testing.T, s *RemoteService, id, lease, body string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	got, err := s.WaitForIngress(ctx, id, lease)
	if err != nil || got.Body != body {
		t.Fatal("accepted message was lost or delivered out of order")
	}
}
func expiryQueueEmpty(t *testing.T, s *RemoteService, id, lease string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if _, err := s.WaitForIngress(ctx, id, lease); err != context.DeadlineExceeded {
		t.Fatal("expected an empty live queue")
	}
}

func TestIngressExpiryQueueResumesExactlyOnceInFIFOOrder(t *testing.T) {
	s, now, req, registered := expiryQueueFixture(t)
	expiryQueueSend(t, s, "before expiry")
	*now = now.Add(remoteLeaseDuration + time.Second)
	s.sweep()
	missing, _ := s.GetSession(t.Context(), registered.RegistrationID)
	if missing.PendingMessages != 1 || missing.Status != StatusMissing {
		t.Error("expired ingress lost its accepted message")
	}
	listed, err := s.ListSessions(t.Context())
	if err != nil || len(listed) != 1 || listed[0].PendingMessages != 1 {
		t.Error("roster omitted held messages")
	}

	if _, err := s.SendMessage(t.Context(), Message{From: "recipient", To: "recipient", Body: "while missing"}); err == nil {
		t.Fatal("retired ingress accepted another send")
	}
	resumed := expiryQueueResume(t, s, req, registered)
	expiryQueueSend(t, s, "after resume")
	expiryQueueReceive(t, s, resumed.RegistrationID, "resumed-lease", "before expiry")
	expiryQueueReceive(t, s, resumed.RegistrationID, "resumed-lease", "after resume")
	expiryQueueEmpty(t, s, resumed.RegistrationID, "resumed-lease")
}

func TestIngressExpiryQueueTenMinuteBoundaryUsesLeaseDeadline(t *testing.T) {
	for _, elapsed := range []time.Duration{10*time.Minute - time.Nanosecond, 10 * time.Minute, 11 * time.Minute} {
		t.Run(elapsed.String(), func(t *testing.T) {
			s, now, req, registered := expiryQueueFixture(t)
			expiryQueueSend(t, s, "held")
			if elapsed == 10*time.Minute {
				*now = now.Add(remoteLeaseDuration)
				s.sweep()
				held, _ := s.GetSession(t.Context(), registered.RegistrationID)
				if held.PendingMessages != 1 {
					t.Fatal("initial expiry lost queue")
				}
				*now = now.Add(elapsed)
			} else {
				*now = now.Add(remoteLeaseDuration + elapsed)
			}
			// First sweep can be late; TTL starts at the lease deadline, not this sweep.
			s.sweep()
			want := 0
			if elapsed < 10*time.Minute {
				want = 1
			}
			snapshot, _ := s.GetSession(t.Context(), registered.RegistrationID)
			if snapshot.PendingMessages != want {
				t.Fatalf("held count=%d want=%d", snapshot.PendingMessages, want)
			}
			resumed := expiryQueueResume(t, s, req, registered)
			if want == 1 {
				expiryQueueReceive(t, s, resumed.RegistrationID, "resumed-lease", "held")
			}
			expiryQueueEmpty(t, s, resumed.RegistrationID, "resumed-lease")
		})
	}
}

func TestIngressExpiryQueueDiscardsOnTerminalTransitions(t *testing.T) {
	for _, kind := range []string{"detach", "suspend", "ingress-failed", "identity-mismatch", "probe-unanswered"} {
		t.Run(kind, func(t *testing.T) {
			s, now, req, registered := expiryQueueFixture(t)
			expiryQueueSend(t, s, "must discard")
			*now = now.Add(remoteLeaseDuration + time.Second)
			s.sweep()
			switch kind {
			case "detach":
				if err := s.StopSession(t.Context(), registered.RegistrationID); err != nil {
					t.Fatal(err)
				}
			case "suspend":
				if err := s.SetSuspended(t.Context(), registered.RegistrationID, true); err != nil {
					t.Fatal(err)
				}
			case "ingress-failed":
				if err := s.ReportIngressFailure(t.Context(), registered.RegistrationID, req.ChannelID); err != nil {
					t.Fatal(err)
				}
			case "identity-mismatch", "probe-unanswered":
				// Drive a real pending probe and its response/timeout while queue TTL is live.
				s.SetProbeSchedule(time.Second, time.Second, 0)
				s.sweep()
				if kind == "identity-mismatch" {
					expiryQueueResume(t, s, req, registered)
					e := s.byRegistration[registered.RegistrationID]
					result, err := s.AnswerProbe(t.Context(), ProbeAnswer{RegistrationID: registered.RegistrationID, ChannelID: "resumed-lease", ProbeID: e.record.Probe.ID, Revision: e.record.Revision, OK: false})
					if err != nil || result.Code != "" {
						t.Fatal("probe refusal failed")
					}
				} else {
					*now = now.Add(time.Second)
					s.sweep()
				}
			}
			got, _ := s.GetSession(t.Context(), registered.RegistrationID)
			if got.PendingMessages != 0 {
				t.Fatal("terminal transition retained accepted messages")
			}
		})
	}
}

func TestIngressExpiryQueueAlreadyDequeuedIsNotReplayed(t *testing.T) {
	s, now, req, registered := expiryQueueFixture(t)
	expiryQueueSend(t, s, "consumed")
	expiryQueueReceive(t, s, registered.RegistrationID, req.ChannelID, "consumed")
	*now = now.Add(remoteLeaseDuration + time.Second)
	s.sweep()
	resumed := expiryQueueResume(t, s, req, registered)
	expiryQueueEmpty(t, s, resumed.RegistrationID, "resumed-lease")
}

func TestIngressExpiryQueueResumedQueueStillHasThirtyTwoCap(t *testing.T) {
	s, now, req, registered := expiryQueueFixture(t)
	for n := 0; n < 32; n++ {
		expiryQueueSend(t, s, fmt.Sprint(n))
	}
	*now = now.Add(remoteLeaseDuration + time.Second)
	s.sweep()
	resumed := expiryQueueResume(t, s, req, registered)
	if _, err := s.SendMessage(t.Context(), Message{From: "recipient", To: "recipient", Body: "overflow"}); err != ErrQueueFull {
		t.Fatal("held queue bypassed 32 cap")
	}
	for n := 0; n < 32; n++ {
		expiryQueueReceive(t, s, resumed.RegistrationID, "resumed-lease", fmt.Sprint(n))
	}
	expiryQueueEmpty(t, s, resumed.RegistrationID, "resumed-lease")
}

func TestIngressExpiryQueueNeverMovesToAnotherRegistrationWithSameName(t *testing.T) {
	s, now, _, registered := expiryQueueFixture(t)
	expiryQueueSend(t, s, "old registration only")
	*now = now.Add(remoteLeaseDuration + time.Second)
	s.sweep()
	if err := s.StopSession(t.Context(), registered.RegistrationID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveDetachedSession(t.Context(), registered.RegistrationID, nil); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.RegisterSession(t.Context(), durableRegister(2, "recipient", "other-client", durableSecretB, "other-lease", "other-register"))
	if err != nil || replacement.Code != "" || replacement.Session.RegistrationID == registered.RegistrationID {
		t.Fatal("replacement fixture failed")
	}
	expiryQueueEmpty(t, s, replacement.Session.RegistrationID, "other-lease")
}

func TestIngressExpiryQueueIsMemoryOnlyAcrossServerRestart(t *testing.T) {
	root := t.TempDir()
	s, now := clockedService(t, root)
	req := durableRegister(1, "recipient", "expiry-client", durableSecretA, "lease", "register")
	registered, err := s.RegisterSession(t.Context(), req)
	if err != nil || registered.Code != "" {
		t.Fatal("fixture registration failed")
	}
	expiryQueueSend(t, s, "not durable")
	*now = now.Add(remoteLeaseDuration + time.Second)
	s.sweep()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := newRemoteService(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	resumed := expiryQueueResume(t, restored, req, registered.Session)
	expiryQueueEmpty(t, restored, resumed.RegistrationID, "resumed-lease")
}
