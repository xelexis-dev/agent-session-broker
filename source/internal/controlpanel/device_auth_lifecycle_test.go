package controlpanel

import (
	"errors"
	"testing"
	"time"
)

type deviceFixture struct {
	store   *Store
	now     time.Time
	project string
	raw     string
}

func newDeviceFixture(t *testing.T) *deviceFixture {
	t.Helper()
	s := userStore(t)
	f := &deviceFixture{store: s, now: time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC), project: "demo"}
	s.now = func() time.Time { return f.now }
	raw, err := s.IssueProjectTokenFor("owner", "demo", "shared harnesses")
	if err != nil {
		t.Fatal(err)
	}
	f.raw = raw
	return f
}

func (f *deviceFixture) resetPoll() {
	f.store.mu.Lock()
	f.store.devicePoll = nil
	f.store.mu.Unlock()
}

// approve runs one full device authorization for a conversation and returns
// the exchanged session credential.
func (f *deviceFixture) approve(t *testing.T, actor, session, nickname string) (DeviceTokenResponse, DeviceRequest) {
	t.Helper()
	req := DeviceRequest{ClientID: "asb-client-v1", Harness: "claude", SessionID: session, InstanceID: "pc-example", RequestID: "request-" + session}
	code, err := f.store.BeginDeviceAuthorization(f.project, f.raw, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ApproveDeviceAuthorization(actor, code.UserCode, nickname, false); err != nil {
		t.Fatal(err)
	}
	f.resetPoll()
	granted, err := f.store.ExchangeDeviceToken(f.project, f.raw, code.DeviceCode, req.ClientID, false)
	if err != nil {
		t.Fatal(err)
	}
	return granted, req
}

func TestProjectTokenAndSessionGrantOutliveLegacyUnusedExpiry(t *testing.T) {
	f := newDeviceFixture(t)
	f.now = f.now.Add(31 * time.Minute)
	if err := f.store.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	granted, _ := f.approve(t, "owner", "11111111-1111-4111-8111-111111111111", "agent-a")
	f.now = f.now.Add(31 * time.Minute)
	if err := f.store.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	if !f.store.Authorize("demo", granted.AccessToken) {
		t.Fatal("approved session credential expired under the legacy 30 minute rule")
	}
	if _, err := f.store.BeginDeviceAuthorization("demo", f.raw, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "22222222-2222-4222-8222-222222222222", InstanceID: "pc-example", RequestID: "request-b"}); err != nil {
		t.Fatalf("project token expired under the legacy rule: %v", err)
	}
	// Idle lifetime: never used since issue → gone after seven days.
	f.now = f.now.Add(7*24*time.Hour + time.Minute)
	if err := f.store.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	if f.store.Authorize("demo", granted.AccessToken) {
		t.Fatal("idle session credential still authorizes")
	}
	for _, token := range f.store.Projects()[0].Tokens {
		if token.Kind == tokenSession && tokenReservesName(token, token.Nickname) {
			t.Fatal("idle session grant still reserves its nickname")
		}
	}
	// Absolute lifetime: regular use does not extend past thirty days.
	granted, _ = f.approve(t, "owner", "33333333-3333-4333-8333-333333333333", "agent-c")
	for day := 0; day < 29; day++ {
		f.now = f.now.Add(24 * time.Hour)
		if !f.store.Authorize("demo", granted.AccessToken) {
			t.Fatalf("active credential rejected on day %d", day+1)
		}
	}
	f.now = f.now.Add(2 * 24 * time.Hour)
	if f.store.Authorize("demo", granted.AccessToken) {
		t.Fatal("session credential outlived its absolute lifetime")
	}
	if err := f.store.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	for _, token := range f.store.Projects()[0].Tokens {
		if token.Kind == tokenSession && token.ExpiredAt.IsZero() {
			t.Fatal("expired session grant was not marked inactive")
		}
	}
}

func TestProjectsForHidesDeviceRecordsAndDescribesTokenKinds(t *testing.T) {
	f := newDeviceFixture(t)
	if _, err := f.store.BeginDeviceAuthorization("demo", f.raw, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "22222222-2222-4222-8222-222222222222", InstanceID: "pc-example", RequestID: "request-b"}); err != nil {
		t.Fatal(err)
	}
	granted, _ := f.approve(t, "owner", "11111111-1111-4111-8111-111111111111", "agent-a")
	for _, actor := range []string{"owner", "admin", "developer"} {
		for _, p := range f.store.ProjectsFor(actor) {
			if p.Devices != nil {
				t.Fatalf("%s sees device authorization records", actor)
			}
			for _, token := range p.Tokens {
				if token.TokenHash != "" {
					t.Fatalf("%s sees a token hash", actor)
				}
				if token.Kind == tokenSession && (token.ParentID == "" || token.ExpiresAt.IsZero() || token.Nickname != "agent-a") {
					t.Fatalf("session grant lost its description: %+v", token)
				}
				if token.Kind == tokenProject && token.Label != "shared harnesses" {
					t.Fatalf("project token lost its label: %+v", token)
				}
			}
		}
	}
	if !f.store.Authorize("demo", granted.AccessToken) {
		t.Fatal("listing invalidated the credential")
	}
}

func TestRevokingProjectTokenCascadesToGrantsAndClosesTheirStreams(t *testing.T) {
	f := newDeviceFixture(t)
	granted, _ := f.approve(t, "owner", "11111111-1111-4111-8111-111111111111", "agent-a")
	grant, _, ok := f.store.authorize("demo", granted.AccessToken)
	if !ok || grant.Revoked == nil || grant.ExpiresAt.IsZero() {
		t.Fatalf("session grant lacks lifetime signals: %+v", grant)
	}
	var parentID string
	for _, token := range f.store.Projects()[0].Tokens {
		if token.Kind == tokenProject {
			parentID = token.ID
		}
	}
	if err := f.store.RevokeTokenFor("owner", "demo", parentID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-grant.Revoked:
	default:
		t.Fatal("revoking the parent did not signal the session stream")
	}
	if f.store.Authorize("demo", granted.AccessToken) {
		t.Fatal("child credential survives parent revocation")
	}
	for _, token := range f.store.Projects()[0].Tokens {
		if !token.Revoked {
			t.Fatalf("token %s (%s) not revoked with its parent", token.ID, token.Kind)
		}
	}
	if _, err := f.store.BeginDeviceAuthorization("demo", f.raw, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "22222222-2222-4222-8222-222222222222", InstanceID: "pc-example", RequestID: "request-b"}); !errors.Is(err, errDeviceClient) {
		t.Fatalf("revoked project token still issues codes: %v", err)
	}
	// The nickname is free again for a new approval.
	second, err := f.store.IssueProjectTokenFor("owner", "demo", "replacement")
	if err != nil {
		t.Fatal(err)
	}
	f.raw = second
	if _, req := f.approve(t, "owner", "44444444-4444-4444-8444-444444444444", "agent-a"); req.SessionID == "" {
		t.Fatal("nickname not released")
	}
}

func TestDetachingASessionGrantKeepsParentAndSiblingsUsable(t *testing.T) {
	f := newDeviceFixture(t)
	first, firstReq := f.approve(t, "owner", "11111111-1111-4111-8111-111111111111", "agent-a")
	second, _ := f.approve(t, "owner", "22222222-2222-4222-8222-222222222222", "agent-b")
	if err := f.store.RevokeSessionTokens("demo", "agent-a", firstReq.identity()); err != nil {
		t.Fatal(err)
	}
	if f.store.Authorize("demo", first.AccessToken) || !f.store.Authorize("demo", second.AccessToken) {
		t.Fatal("detach touched the wrong grant")
	}
	if _, err := f.store.BeginDeviceAuthorization("demo", f.raw, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "33333333-3333-4333-8333-333333333333", InstanceID: "pc-example", RequestID: "request-c"}); err != nil {
		t.Fatalf("parent token unusable after a grant detach: %v", err)
	}
}

func TestProjectDeletionClearsDeviceAuthorizations(t *testing.T) {
	f := newDeviceFixture(t)
	if _, err := f.store.BeginDeviceAuthorization("demo", f.raw, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "22222222-2222-4222-8222-222222222222", InstanceID: "pc-example", RequestID: "request-b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.beginProjectDeletion("owner", "demo", "demo"); err != nil {
		t.Fatal(err)
	}
	f.store.mu.Lock()
	p := f.store.data.Projects["demo"]
	valid := validState(f.store.data)
	f.store.mu.Unlock()
	if len(p.Devices) != 0 || len(p.Tokens) != 0 || !valid {
		t.Fatalf("deleting project keeps device records: devices=%d tokens=%d valid=%v", len(p.Devices), len(p.Tokens), valid)
	}
}
