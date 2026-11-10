package controlpanel

import (
	"errors"
	"testing"
)

func TestRevokeSessionTokensTargetsBindingAndPreservesStateOnFailure(t *testing.T) {
	s := userStore(t)
	raw, err := s.IssueTokenFor("developer", "demo", "reusable", "")
	if err != nil {
		t.Fatal(err)
	}
	g, _, ok := s.authorize("demo", raw)
	if !ok || g.Claim("old-identity") != nil {
		t.Fatal("claim")
	}
	if err := s.RevokeSessionTokens("demo", "reusable", "other-identity"); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSessionTokens("demo", "different-name", "old-identity"); err != nil {
		t.Fatal(err)
	}
	if !s.Authorize("demo", raw) {
		t.Fatal("unrelated binding revoked")
	}
	write := s.write
	s.write = func([]byte) error { return errors.New("write failed") }
	if err := s.RevokeSessionTokens("demo", "reusable", "old-identity"); err == nil {
		t.Fatal("failed write reported success")
	}
	s.write = write
	if !s.Authorize("demo", raw) {
		t.Fatal("failed revocation published")
	}
	if err := s.RevokeSessionTokens("demo", "reusable", "old-identity"); err != nil {
		t.Fatal(err)
	}
	if s.Authorize("demo", raw) || g.Check("old-identity") == nil {
		t.Fatal("revoked credential remains authorized")
	}
	replacement, err := s.IssueTokenFor("developer", "demo", "reusable", "")
	if err != nil {
		t.Fatal("name not released", err)
	}
	if err := s.RevokeSessionTokens("demo", "reusable", "old-identity"); err != nil {
		t.Fatal(err)
	}
	if !s.Authorize("demo", replacement) {
		t.Fatal("cleanup retry revoked unbound replacement")
	}
}

func TestPanelRevokeSessionTokensNotifiesOnlyCommittedAffectedToken(t *testing.T) {
	f := detachedCleanupFixture(t)
	raw, err := f.store.IssueToken("demo", "unrelated-panel-watcher")
	if err != nil {
		t.Fatal("unrelated token")
	}
	unrelated := directGrant(t, f.store, raw)
	write := f.store.writeBatch
	f.store.writeBatch = func(map[string][]byte, []string) error { return ErrStorage }
	if err := f.store.RevokeSessionTokens("demo", f.proof.Name, f.proof.Identity); err == nil {
		t.Fatal("failed revocation reported success")
	}
	select {
	case <-f.grant.Revoked:
		t.Fatal("failed policy commit notified live token")
	default:
	}
	f.store.writeBatch = write
	if err := f.store.RevokeSessionTokens("demo", f.proof.Name, f.proof.Identity); err != nil {
		t.Fatal("panel revocation")
	}
	select {
	case <-f.grant.Revoked:
	default:
		t.Fatal("panel-revoked token user was not notified")
	}
	select {
	case <-unrelated.Revoked:
		t.Fatal("unrelated token user was notified")
	default:
	}
}
