package controlpanel

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestTokenLifetimeBoundaryAndFirstClaim(t *testing.T) {
	s := userStore(t)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	raw, err := s.IssueTokenFor("developer", "demo", "expiry", "developer")
	if err != nil {
		t.Fatal(err)
	}
	g, _, ok := s.authorize("demo", raw)
	if !ok {
		t.Fatal("unauthorized")
	}
	now = now.Add(30*time.Minute - time.Nanosecond)
	if !s.Authorize("demo", raw) {
		t.Fatal("early expiration")
	}
	now = now.Add(time.Nanosecond)
	if s.Authorize("demo", raw) || g.Claim("codex:s:c") == nil {
		t.Fatal("expired grant revived")
	}
	raw, err = s.IssueTokenFor("developer", "demo", "expiry", "developer")
	if err != nil {
		t.Fatal("nickname not freed", err)
	}
	g, _, _ = s.authorize("demo", raw)
	if err = g.Claim("codex:s:c"); err != nil {
		t.Fatal(err)
	}
	if g.Connected == nil {
		t.Fatal("registration confirmation callback absent")
	}
	if err = g.Connected("codex:s:c"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if !s.Authorize("demo", raw) {
		t.Fatal("connected token expired")
	}
	if s.ProjectsFor("developer")[0].Tokens[1].FirstConnectedAt.IsZero() {
		t.Fatal("first connection missing")
	}
}

func TestTokenLifetimeLegacyMigrationAndRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProject("demo", "token", true, ""); err != nil {
		t.Fatal(err)
	}
	raw, err := s.IssueToken("demo", "old")
	if err != nil {
		t.Fatal(err)
	}
	old := s.data
	old.Version = 1
	old.Users = nil
	p := old.Projects["demo"]
	p.ProjectGenerationID = ""
	p.Members = nil
	p.Tokens[0].ScopeVersion = 0
	p.Tokens[0].Scopes = nil
	p.Tokens[0].OwnerID = ""
	p.Tokens[0].IssuedAt = time.Time{}
	old.Projects["demo"] = p
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(old)
	dir = t.TempDir() // Import into a fresh SQLite store.
	if err = os.WriteFile(filepath.Join(dir, "control.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	p = s.Projects()[0]
	issued := p.Tokens[0].IssuedAt
	if p.Members["admin"] != "owner" || p.Tokens[0].OwnerID != "admin" || issued.IsZero() || !s.Authenticate(testPassword) {
		t.Fatal("legacy migration failed")
	}
	s.Close()
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.Projects()[0].Tokens[0].IssuedAt.Equal(issued) {
		t.Fatal("restart extended lifetime")
	}
	s.now = func() time.Time { return issued.Add(30 * time.Minute) }
	if s.Authorize("demo", raw) {
		t.Fatal("restart revived expiry")
	}
}

func TestTokenLifetimeClaimWithoutSuccessfulRegistrationStillExpires(t *testing.T) {
	s := userStore(t)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	raw, err := s.IssueTokenFor("developer", "demo", "attempt", "developer")
	if err != nil {
		t.Fatal(err)
	}
	g, _, _ := s.authorize("demo", raw)
	if err = g.Claim(`["codex","session","client"]`); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Minute)
	if s.Authorize("demo", raw) {
		t.Fatal("claim without successful registration bypassed expiration")
	}
}

func TestTokenLifetimeFailedSweepRetainsStateAndCancelsAfterRetry(t *testing.T) {
	s := userStore(t)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	raw, err := s.IssueTokenFor("developer", "demo", "attempt", "developer")
	if err != nil {
		t.Fatal(err)
	}
	_, changes, _ := s.authorize("demo", raw)
	write := s.write
	s.write = func([]byte) error { return errors.New("unavailable") }
	now = now.Add(30 * time.Minute)
	if s.SweepExpired() == nil || s.Authorize("demo", raw) {
		t.Fatal("expiration failed open")
	}
	if len(s.data.Projects["demo"].Tokens) != 1 {
		t.Fatal("failed expiration published")
	}
	select {
	case <-changes:
		t.Fatal("failed write cancelled generation")
	default:
	}
	s.write = write
	if err = s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	default:
		t.Fatal("expiration did not cancel generation")
	}
	if len(s.Projects()[0].Tokens) != 1 || s.Projects()[0].Tokens[0].ExpiredAt.IsZero() {
		t.Fatal("expired history missing")
	}
}

func TestTokenLifetimeConcurrentExpiryAndClaim(t *testing.T) {
	s := userStore(t)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	raw, err := s.IssueTokenFor("developer", "demo", "race", "developer")
	if err != nil {
		t.Fatal(err)
	}
	g, _, _ := s.authorize("demo", raw)
	now = now.Add(30 * time.Minute)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if g.Claim(`["codex","s","c"]`) == nil {
			t.Error("claim won past expiration")
		}
	}()
	go func() {
		defer wg.Done()
		if err := s.SweepExpired(); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
	if len(s.Projects()[0].Tokens) != 1 || s.Authorize("demo", raw) {
		t.Fatal("expiration race lost history or revived token")
	}
}

func TestTokenLifetimeConfirmationChecksBindingAndRollsBack(t *testing.T) {
	s := userStore(t)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	raw, err := s.IssueTokenFor("developer", "demo", "confirm", "developer")
	if err != nil {
		t.Fatal(err)
	}
	g, _, _ := s.authorize("demo", raw)
	if g.Connected == nil {
		t.Fatal("registration confirmation callback absent")
	}
	identity := `["codex","s","c"]`
	if g.Connected(identity) == nil {
		t.Fatal("confirmed without claim")
	}
	if err = g.Claim(identity); err != nil {
		t.Fatal(err)
	}
	if g.Connected(`["codex","other","c"]`) == nil {
		t.Fatal("confirmed different identity")
	}
	write := s.write
	s.write = func([]byte) error { return errors.New("unavailable") }
	if g.Connected(identity) == nil {
		t.Fatal("confirmed without persistence")
	}
	if !s.data.Projects["demo"].Tokens[0].FirstConnectedAt.IsZero() {
		t.Fatal("failed confirmation published")
	}
	s.write = write
	if err = g.Connected(identity); err != nil {
		t.Fatal(err)
	}
	first := s.Projects()[0].Tokens[0].FirstConnectedAt
	now = now.Add(time.Hour)
	if err = g.Connected(identity); err != nil {
		t.Fatal(err)
	}
	if !s.Projects()[0].Tokens[0].FirstConnectedAt.Equal(first) {
		t.Fatal("confirmation timestamp reset")
	}
}
