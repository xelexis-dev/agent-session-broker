package broker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func setSuspend(t *testing.T, s *RemoteService, ctx context.Context, id string, suspended bool) error {
	t.Helper()
	setter, ok := any(s).(interface {
		SetSuspended(context.Context, string, bool) error
	})
	if !ok {
		t.Fatal("RemoteService does not implement durable suspension")
	}
	return setter.SetSuspended(ctx, id, suspended)
}

func TestSuspendPersistsDiscardsQueueAndRequiresFreshLease(t *testing.T) {
	ctx := context.Background()
	for _, sqlite := range []bool{false, true} {
		name := "json"
		if sqlite {
			name = "sqlite"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			s, err := newRemoteServiceWithStore(root, false, sqlite)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			a := durableRegister(1, "alpha", "client-alpha-0001", durableSecretA, "lease-1", "request-1")
			first, err := s.RegisterSession(ctx, a)
			if err != nil || first.Code != "" {
				t.Fatalf("register: %+v %v", first, err)
			}
			id := first.Session.RegistrationID
			if _, err := s.SendMessage(ctx, Message{From: "alpha", To: "alpha", Body: "discard me"}); err != nil {
				t.Fatal(err)
			}
			if err := setSuspend(t, s, ctx, id, true); err != nil {
				t.Fatal(err)
			}
			got, _ := s.GetSession(ctx, id)
			if got.Status != "suspended" || got.AutomaticDeliveryReady || got.PendingMessages != 0 || got.Revision != first.Session.Revision+1 || got.Name != "alpha" {
				t.Fatalf("suspended: %+v", got)
			}
			if replay, _ := s.RegisterSession(ctx, a); replay.Code != "suspended" {
				t.Fatalf("register replay bypassed suspension: %+v", replay)
			}
			if resumed, _ := s.ResumeSession(ctx, resumeFrom(a, id, "lease-2", "request-2", got.Revision)); resumed.Code != "suspended" {
				t.Fatalf("resume bypassed suspension: %+v", resumed)
			}
			if renewed, _ := s.RenewLease(ctx, RenewRequest{RegistrationID: id, ChannelID: a.ChannelID}); renewed.Code != "suspended" {
				t.Fatalf("renew bypassed suspension: %+v", renewed)
			}
			if probe, _ := s.AnswerProbe(ctx, ProbeAnswer{RegistrationID: id, ChannelID: a.ChannelID, ProbeID: "stale", OK: false}); probe.Code != "suspended" {
				t.Fatalf("probe bypassed suspension: %+v", probe)
			}
			if err := s.StopSession(ctx, id); err == nil {
				t.Fatal("stop changed suspended registration")
			}
			if err := s.ReportIngressFailure(ctx, id, a.ChannelID); err == nil {
				t.Fatal("stale ingress failure accepted")
			}
			if _, err := s.WaitForIngress(ctx, id, a.ChannelID); err == nil {
				t.Fatal("suspended receiver accepted")
			}
			if err := setSuspend(t, s, ctx, id, false); err != nil {
				t.Fatal(err)
			}
			got, _ = s.GetSession(ctx, id)
			if got.Status != StatusMissing || got.AutomaticDeliveryReady {
				t.Fatalf("unsuspend invented liveness: %+v", got)
			}
			if resumed, _ := s.ResumeSession(ctx, resumeFrom(a, id, a.ChannelID, "old-lease", got.Revision)); resumed.Code != CodeInvalid {
				t.Fatalf("old lease revived: %+v", resumed)
			}
			if err := setSuspend(t, s, ctx, id, true); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			restored, err := newRemoteServiceWithStore(root, false, sqlite)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			later := time.Now().Add(365 * 24 * time.Hour)
			restored.now = func() time.Time { return later }
			restored.SetProbeSchedule(time.Second, time.Second, 0)
			for i := 0; i < 3; i++ {
				restored.sweep()
				later = later.Add(24 * time.Hour)
			}
			got, err = restored.GetSession(ctx, id)
			if err != nil || got.Status != "suspended" || got.ProbePending || got.Name != "alpha" {
				t.Fatalf("restart/TTL lost policy: %+v %v", got, err)
			}
			if err := setSuspend(t, restored, ctx, id, false); err != nil {
				t.Fatal(err)
			}
			got, _ = restored.GetSession(ctx, id)
			if got.ProbePending {
				t.Fatal("unsuspend did not reset cleanup grace")
			}
			if reused, _ := restored.ResumeSession(ctx, resumeFrom(a, id, a.ChannelID, "reused-after-restart", got.Revision)); reused.Code != CodeInvalid {
				t.Fatalf("restart forgot suspended lease fence: %+v", reused)
			}
			resumed, err := restored.ResumeSession(ctx, resumeFrom(a, id, "fresh-lease", "fresh-request", got.Revision))
			if err != nil || resumed.Code != "" || resumed.Session.Status != StatusAlive || resumed.Session.PendingMessages != 0 {
				t.Fatalf("fresh resume: %+v %v", resumed, err)
			}
			if _, err := restored.SendMessage(ctx, Message{From: "alpha", To: "alpha", Body: "new"}); err != nil {
				t.Fatal(err)
			}
			message, err := restored.WaitForIngress(ctx, id, "fresh-lease")
			if err != nil || message.Body != "new" {
				t.Fatalf("discarded message returned: %+v %v", message, err)
			}
		})
	}
}

type suspendClaimAdmission struct {
	claim func()
	check func()
}

func (a *suspendClaimAdmission) CheckSession(context.Context, Session) error {
	if a.check != nil {
		a.check()
	}
	return nil
}

func (a *suspendClaimAdmission) Admit(_ context.Context, r RegisterRequest) (string, string, error) {
	return r.Name, "", nil
}

func (a *suspendClaimAdmission) Claim(context.Context, RegisterRequest) (string, error) {
	a.claim()
	return "", nil
}

func TestSuspendDuringAdmissionClaimCannotBeOverwritten(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, _ := clockedService(t, "")
	a := durableRegister(1, "alpha", "client-alpha-0001", durableSecretA, "lease-a", "req-a")
	first, _ := s.RegisterSession(ctx, a)
	claiming, release := make(chan struct{}), make(chan struct{})
	s.SetAdmission(&suspendClaimAdmission{claim: func() { close(claiming); <-release }})
	a.RequestID, a.ChannelID, a.ExpectedRevision = "new-request", "new-lease", first.Session.Revision
	done := make(chan RegisterResult, 1)
	go func() { result, _ := s.RegisterSession(ctx, a); done <- result }()
	select {
	case <-claiming:
	case <-ctx.Done():
		t.Fatal("register did not reach admission claim")
	}
	err := setSuspend(t, s, ctx, "alpha", true)
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.Code != "suspended" {
			t.Fatalf("post-claim register bypassed suspend: %+v", result)
		}
	case <-ctx.Done():
		t.Fatal("post-claim register did not return")
	}
	got, _ := s.GetSession(ctx, "alpha")
	if got.Status != "suspended" || got.Revision != 2 || got.AutomaticDeliveryReady {
		t.Fatalf("claim race changed policy: %+v", got)
	}
}

func TestSuspendClearsPendingProbeAndResumeReplay(t *testing.T) {
	ctx := context.Background()
	s, now := clockedService(t, "")
	s.SetProbeSchedule(time.Minute, time.Minute, 0)
	a := durableRegister(1, "alpha", "client-alpha-0001", durableSecretA, "lease-a", "req-a")
	first, _ := s.RegisterSession(ctx, a)
	*now = now.Add(2 * time.Minute)
	s.sweep()
	missing, _ := s.GetSession(ctx, "alpha")
	if !missing.ProbePending {
		t.Fatal("missing fixture has no probe")
	}
	r := resumeFrom(a, first.Session.RegistrationID, "lease-b", "resume-b", first.Session.Revision)
	resumed, _ := s.ResumeSession(ctx, r)
	if resumed.Code != "" || resumed.PendingProbe == nil {
		t.Fatalf("resume fixture: %+v", resumed)
	}
	if err := setSuspend(t, s, ctx, "alpha", true); err != nil {
		t.Fatal(err)
	}
	if replayed, _ := s.ResumeSession(ctx, r); replayed.Code != "suspended" {
		t.Fatalf("resume replay bypassed suspension: %+v", replayed)
	}
	*now = now.Add(24 * time.Hour)
	got, err := s.GetSession(ctx, "alpha")
	if err != nil || got.ProbePending || got.Status != "suspended" {
		t.Fatalf("old probe deleted suspension: %+v %v", got, err)
	}
	if err := setSuspend(t, s, ctx, "alpha", true); err != nil {
		t.Fatal(err)
	}
	again, _ := s.GetSession(ctx, "alpha")
	if again.Revision != got.Revision {
		t.Fatalf("duplicate suspend changed revision: %+v", again)
	}
}

func TestSuspendDeniesBothDirectionsAndAccessGrants(t *testing.T) {
	ctx := context.Background()
	s, _ := clockedService(t, "")
	a := durableRegister(1, "alpha", "client-alpha-0001", durableSecretA, "lease-a", "req-a")
	b := durableRegister(2, "beta", "client-beta-0002", durableSecretB, "lease-b", "req-b")
	s.RegisterSession(ctx, a)
	s.RegisterSession(ctx, b)
	if err := setSuspend(t, s, ctx, "alpha", true); err != nil {
		t.Fatal(err)
	}
	for _, m := range []Message{{From: "alpha", To: "beta", Body: "blocked"}, {From: "beta", To: "alpha", Body: "blocked"}} {
		if _, err := s.SendMessage(ctx, m); err == nil {
			t.Fatal("suspended send accepted")
		}
	}
	if err := setSuspend(t, s, access.WithGrant(ctx, access.Grant{Nickname: "alpha"}), "alpha", false); err == nil {
		t.Fatal("token grant lifted management policy")
	}
	got, _ := s.GetSession(ctx, "alpha")
	if got.Status != "suspended" {
		t.Fatalf("grant changed policy: %+v", got)
	}
	if _, err := s.SendMessage(ctx, Message{From: "beta", To: "beta", Body: "unaffected"}); err != nil {
		t.Fatal(err)
	}
}

func TestSuspendStorageFailureRollsBackPolicyLeaseAndQueue(t *testing.T) {
	ctx := context.Background()
	s, _ := clockedService(t, t.TempDir())
	a := durableRegister(1, "alpha", "client-alpha-0001", durableSecretA, "lease-a", "req-a")
	first, _ := s.RegisterSession(ctx, a)
	s.SendMessage(ctx, Message{From: "alpha", To: "alpha", Body: "preserved"})
	s.store.writeFile = func(string, []byte) error { return errors.New("injected storage failure") }
	if err := setSuspend(t, s, ctx, "alpha", true); err == nil {
		t.Fatal("failed write reported success")
	}
	got, _ := s.GetSession(ctx, "alpha")
	if got.Revision != first.Session.Revision || got.Status != StatusAlive || got.PendingMessages != 1 {
		t.Fatalf("failed write mutated memory: %+v", got)
	}
	m, err := s.WaitForIngress(ctx, "alpha", a.ChannelID)
	if err != nil || m.Body != "preserved" {
		t.Fatalf("failed write retired receiver: %+v %v", m, err)
	}
	s.store.writeFile = nil
	if err := setSuspend(t, s, ctx, "alpha", true); err != nil {
		t.Fatal(err)
	}
	s.store.writeFile = func(string, []byte) error { return errors.New("injected storage failure") }
	if err := setSuspend(t, s, ctx, "alpha", false); err == nil {
		t.Fatal("failed unsuspend reported success")
	}
	got, _ = s.GetSession(ctx, "alpha")
	if got.Status != "suspended" || got.Revision != first.Session.Revision+1 {
		t.Fatalf("failed unsuspend changed policy: %+v", got)
	}
}

func TestSuspendReleasesWaitingReceiver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, _ := clockedService(t, "")
	a := durableRegister(1, "alpha", "client-alpha-0001", durableSecretA, "lease-a", "req-a")
	s.RegisterSession(ctx, a)
	waiting := make(chan struct{}, 1)
	s.SetAdmission(&suspendClaimAdmission{check: func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}})
	done := make(chan error, 1)
	go func() { _, err := s.WaitForIngress(ctx, "alpha", a.ChannelID); done <- err }()
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("receiver did not start waiting")
	}
	if err := setSuspend(t, s, ctx, "alpha", true); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("wait not released by suspend: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("suspend did not release receiver")
	}
}

func TestSuspendCannotMakeDetachedRegistrationResumable(t *testing.T) {
	ctx := context.Background()
	s, _ := clockedService(t, "")
	a := durableRegister(1, "alpha", "client-alpha-0001", durableSecretA, "lease-a", "req-a")
	first, err := s.RegisterSession(ctx, a)
	if err != nil || first.Code != "" {
		t.Fatalf("register: %s %v", first.Code, err)
	}
	id := first.Session.RegistrationID
	if err := s.StopSession(ctx, id); err != nil {
		t.Fatal(err)
	}
	detached, err := s.GetSession(ctx, id)
	if err != nil || detached.ErrorCode != CodeDetached {
		t.Fatalf("detach fixture: %s %v", detached.ErrorCode, err)
	}
	// A stale UI action must not replace an explicit detach with a temporary
	// pause whose release allows automatic recovery of the old registration.
	if err := setSuspend(t, s, ctx, id, true); err == nil {
		t.Error("suspend accepted an explicitly detached registration")
	}
	if err := setSuspend(t, s, ctx, id, false); err != nil {
		t.Fatal(err)
	}
	current, err := s.GetSession(ctx, id)
	if err != nil || current.Suspended || current.ErrorCode != CodeDetached || current.Revision != detached.Revision || current.Name != detached.Name {
		t.Errorf("suspend toggle erased detach: code=%s suspended=%v revision=%d want=%d err=%v", current.ErrorCode, current.Suspended, current.Revision, detached.Revision, err)
	}
	resumed, err := s.ResumeSession(ctx, resumeFrom(a, id, "fresh-lease", "fresh-resume", current.Revision))
	if err != nil || resumed.Code != CodeDetached {
		t.Fatalf("detached registration became resumable: code=%s ready=%v err=%v", resumed.Code, resumed.Session.AutomaticDeliveryReady, err)
	}
}
