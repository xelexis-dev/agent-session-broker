package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDurableMissingRegistrationSurvivesUnansweredProbe(t *testing.T) {
	ctx := context.Background()
	s, now := clockedService(t, t.TempDir())
	request := durableRegister(1, "offline", "client-offline-0001", durableSecretA, "lease-1", "req-1")
	first, err := s.RegisterSession(ctx, request)
	if err != nil || first.Code != "" {
		t.Fatalf("register: %s %v", first.Code, err)
	}
	*now = now.Add(time.Hour + time.Minute)
	if got, err := s.GetSession(ctx, "offline"); err != nil || !got.ProbePending {
		t.Fatalf("probe missing: %+v %v", got, err)
	}
	*now = now.Add(10*time.Minute + time.Second)
	listed, err := s.ListSessions(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatalf("unanswered missing registration disappeared from list: count=%d err=%v", len(listed), err)
	}
	if got := listed[0]; got.RegistrationID != first.Session.RegistrationID || got.Name != "offline" || got.Status != StatusMissing || got.AutomaticDeliveryReady {
		t.Fatalf("missing registration changed: %+v", got)
	}
}

func TestDurableCapacityRejectsNewRegistrationWithoutEvictingMissing(t *testing.T) {
	ctx := context.Background()
	s, now := clockedService(t, "")
	var first RegisterResult
	for i := 1; i <= 1024; i++ {
		got, err := s.RegisterSession(ctx, durableRegister(i, fmt.Sprintf("session-%d", i), "client-capacity-0001", durableSecretA, fmt.Sprintf("lease-%d", i), fmt.Sprintf("req-%d", i)))
		if err != nil || got.Code != "" {
			t.Fatalf("register %d: %s %v", i, got.Code, err)
		}
		if i == 1 {
			first = got
		}
	}
	*now = now.Add(2 * time.Hour)
	s.ListSessions(ctx)
	*now = now.Add(time.Hour)
	extra := durableRegister(1025, "extra", "client-capacity-0001", durableSecretA, "lease-extra", "req-extra")
	rejected, err := s.RegisterSession(ctx, extra)
	if err != nil || rejected.Code != CodeCapacity || !strings.Contains(rejected.Warning, "administrator") || !strings.Contains(rejected.Warning, "detach") {
		t.Fatalf("unclear capacity rejection: %+v %v", rejected, err)
	}
	listed, err := s.ListSessions(ctx)
	if err != nil || len(listed) != 1024 {
		t.Fatalf("capacity check evicted registrations: %d %v", len(listed), err)
	}
	request := durableRegister(1, "session-1", "client-capacity-0001", durableSecretA, "lease-1", "req-1")
	resumed, err := s.ResumeSession(ctx, resumeFrom(request, first.Session.RegistrationID, "lease-resume", "req-resume", first.Session.Revision))
	if err != nil || resumed.Code != "" || resumed.Session.RegistrationID != first.Session.RegistrationID {
		t.Fatalf("capacity blocked existing registration resume: %+v %v", resumed, err)
	}
	if err := s.StopSession(ctx, "session-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveDetachedSession(ctx, "session-1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, "session-1"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("explicit detach did not remove: %v", err)
	}
	accepted, err := s.RegisterSession(ctx, extra)
	if err != nil || accepted.Code != "" {
		t.Fatalf("explicit detach did not free capacity: %s %v", accepted.Code, err)
	}
}

func TestSQLiteMissingRegistrationSurvivesLongOfflineRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := newRemoteServiceWithStore(root, false, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.Now()
	s.now = func() time.Time { return now }
	request := durableRegister(1, "offline", "client-offline-0001", durableSecretA, "lease-1", "req-1")
	first, err := s.RegisterSession(ctx, request)
	if err != nil || first.Code != "" {
		t.Fatalf("register: %s %v", first.Code, err)
	}
	now = now.Add(2 * time.Hour)
	s.ListSessions(ctx)
	now = now.Add(24 * time.Hour)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newRemoteServiceWithStore(root, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.now = func() time.Time { return now }
	restarted.bootedAt = now.Add(-time.Hour)
	listed, err := restarted.ListSessions(ctx)
	if err != nil || len(listed) != 1 || listed[0].RegistrationID != first.Session.RegistrationID || listed[0].Status != StatusMissing {
		t.Fatalf("restart lost offline registration: %+v %v", listed, err)
	}
	resumed, err := restarted.ResumeSession(ctx, resumeFrom(request, first.Session.RegistrationID, "lease-2", "req-2", first.Session.Revision))
	if err != nil || resumed.Code != "" || resumed.Session.RegistrationID != first.Session.RegistrationID || resumed.Session.Name != "offline" || !resumed.Session.AutomaticDeliveryReady {
		t.Fatalf("restart resume changed registration: %+v %v", resumed, err)
	}
}
