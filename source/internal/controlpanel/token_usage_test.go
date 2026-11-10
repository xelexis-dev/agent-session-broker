package controlpanel

import (
	"testing"
	"time"
)

func projectTokenOf(t *testing.T, s *Store) SessionToken {
	t.Helper()
	for _, token := range s.ProjectsFor("admin")[0].Tokens {
		if token.Kind == tokenProject {
			return token
		}
	}
	t.Fatal("project token missing")
	return SessionToken{}
}

func TestProjectTokenRecordsUsageApprovalsAndRegistrations(t *testing.T) {
	dir := t.TempDir()
	opener := OpenSQLite
	s, err := opener(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateProject("demo"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 16, 1, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	raw, err := s.IssueProjectTokenFor("admin", "demo", "home")
	if err != nil {
		t.Fatal(err)
	}
	if token := projectTokenOf(t, s); !token.LastUsedAt.IsZero() || token.Approvals != 0 || !token.LastRegisteredAt.IsZero() {
		t.Fatalf("fresh token has history: %+v", token)
	}
	req := DeviceRequest{ClientID: "asb-client-v1", Harness: "claude", SessionID: "conversation-1", InstanceID: "pc-1", RequestID: "request-1"}
	issued, err := s.BeginDeviceAuthorization("demo", raw, req)
	if err != nil {
		t.Fatal(err)
	}
	if token := projectTokenOf(t, s); !token.LastUsedAt.Equal(now) {
		t.Fatalf("code request did not record use: %v", token.LastUsedAt)
	}
	now = now.Add(time.Minute)
	if _, err = s.ApproveDeviceAuthorization("admin", issued.UserCode, "home-claude-1", false); err != nil {
		t.Fatal(err)
	}
	if token := projectTokenOf(t, s); token.Approvals != 1 || !token.LastRegisteredAt.IsZero() {
		t.Fatalf("approval history = %+v", token)
	}
	now = now.Add(time.Minute)
	granted, err := s.ExchangeDeviceToken("demo", raw, issued.DeviceCode, "asb-client-v1", false)
	if err != nil {
		t.Fatal(err)
	}
	if token := projectTokenOf(t, s); !token.LastUsedAt.Equal(now) {
		t.Fatalf("exchange did not record use: %v", token.LastUsedAt)
	}
	grant, _, ok := s.authorize("demo", granted.AccessToken)
	if !ok {
		t.Fatal("session credential rejected")
	}
	now = now.Add(time.Minute)
	if err = grant.Connected(req.identity()); err != nil {
		t.Fatal(err)
	}
	if token := projectTokenOf(t, s); !token.LastRegisteredAt.Equal(now) {
		t.Fatalf("registration did not stamp parent: %v", token.LastRegisteredAt)
	}
	if err = s.RevokeTokenFor("admin", "demo", granted.GrantID); err != nil {
		t.Fatal(err)
	}
	want := projectTokenOf(t, s)
	if want.Approvals != 1 || !want.LastRegisteredAt.Equal(now) {
		t.Fatalf("revocation erased history: %+v", want)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = opener(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := projectTokenOf(t, s)
	if got.Approvals != want.Approvals || !got.LastRegisteredAt.Equal(want.LastRegisteredAt) || !got.LastUsedAt.Equal(want.LastUsedAt) {
		t.Fatalf("history lost on reload: %+v != %+v", got, want)
	}
}
