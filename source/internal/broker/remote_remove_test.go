package broker

import (
	"errors"
	"testing"
)

func TestRemoveDetachedFencesOldLeaseAndPreservesReplacement(t *testing.T) {
	root := t.TempDir()
	s, _ := clockedService(t, root)
	a := durableRegister(1, "reusable", "client-remove-001", durableSecretA, "lease-old", "request-old")
	first, err := s.RegisterSession(t.Context(), a)
	if err != nil || first.Code != "" {
		t.Fatal("register", first.Code, err)
	}
	selector := SelectorRegistration + first.Session.RegistrationID
	if err := s.RemoveDetachedSession(t.Context(), selector, func(Session) error { t.Fatal("live registration cleanup called"); return nil }); err == nil {
		t.Fatal("live registration removed")
	}
	if err := s.StopSession(t.Context(), selector); err != nil {
		t.Fatal(err)
	}
	failed := errors.New("policy write failed")
	if err := s.RemoveDetachedSession(t.Context(), selector, func(Session) error { return failed }); !errors.Is(err, failed) {
		t.Fatal("cleanup failure ignored", err)
	}
	if got, err := s.GetSession(t.Context(), selector); err != nil || got.ErrorCode != CodeDetached {
		t.Fatal("failed cleanup removed registration")
	}
	if err := s.RemoveDetachedSession(t.Context(), selector, nil); err != nil {
		t.Fatal(err)
	}
	b := durableRegister(2, "reusable", "client-remove-002", durableSecretB, "lease-new", "request-new")
	second, err := s.RegisterSession(t.Context(), b)
	if err != nil || second.Code != "" || second.Session.Name != "reusable" {
		t.Fatal("name not released", second.Code, err)
	}
	if renewed, err := s.RenewLease(t.Context(), RenewRequest{RegistrationID: first.Session.RegistrationID, ChannelID: a.ChannelID}); err != nil || renewed.Code != CodeNotFound {
		t.Fatal("old lease renewed", renewed.Code, err)
	}
	if resumed, err := s.ResumeSession(t.Context(), resumeFrom(a, first.Session.RegistrationID, "stale-resume", "stale-request", first.Session.Revision)); err != nil || resumed.Code != CodeNotFound {
		t.Fatal("old registration resumed", resumed.Code, err)
	}
	if err := s.RemoveDetachedSession(t.Context(), selector, nil); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("stale cleanup reached replacement", err)
	}
	if err := s.ReportIngressFailure(t.Context(), "reusable", a.ChannelID); err == nil {
		t.Fatal("old failure retired replacement")
	}
	if got, err := s.GetSession(t.Context(), "reusable"); err != nil || got.RegistrationID != second.Session.RegistrationID || !got.AutomaticDeliveryReady {
		t.Fatal("replacement changed")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := NewPersistentRemoteService(root)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	rows, err := restored.ListSessions(t.Context())
	if err != nil || len(rows) != 1 || rows[0].RegistrationID != second.Session.RegistrationID {
		t.Fatal("file store restored removed registration", err)
	}
}

func TestRemoveDetachedStorageFailureKeepsRegistration(t *testing.T) {
	s, err := NewSQLitePersistentRemoteService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := durableRegister(1, "reusable", "client-remove-001", durableSecretA, "lease-old", "request-old")
	first, err := s.RegisterSession(t.Context(), r)
	if err != nil || first.Code != "" {
		t.Fatal("register", first.Code, err)
	}
	if err := s.StopSession(t.Context(), "reusable"); err != nil {
		t.Fatal(err)
	}
	if err := s.store.database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveDetachedSession(t.Context(), SelectorRegistration+first.Session.RegistrationID, nil); err == nil {
		t.Fatal("failed storage deletion reported success")
	}
	if got, err := s.GetSession(t.Context(), "reusable"); err != nil || got.ErrorCode != CodeDetached {
		t.Fatal("failed deletion lost retryable record")
	}
}

func TestRemoveDetachedLegacyExpiredSession(t *testing.T) {
	s, now := clockedService(t, "")
	r := remoteIdentity(1)
	if _, err := s.AttachSession(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(remoteLeaseDuration + 1)
	if _, err := s.GetSession(t.Context(), r.Name); err != nil {
		t.Fatal(err)
	}
	if err := s.StopSession(t.Context(), r.Name); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveDetachedSession(t.Context(), SelectorName+r.Name, nil); err != nil {
		t.Fatal("expired legacy session cannot be removed", err)
	}
}
