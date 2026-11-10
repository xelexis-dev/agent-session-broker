package controlpanel

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTokenCapRevokedHistoryDoesNotBlockIssue(t *testing.T) {
	s := userStore(t)
	for i := 0; i < 256; i++ {
		if _, err := s.IssueProjectTokenFor("developer", "demo", "history"); err != nil {
			t.Fatal(err)
		}
		p := s.ProjectsFor("developer")[0]
		if err := s.RevokeTokenFor("developer", "demo", p.Tokens[len(p.Tokens)-1].ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.IssueProjectTokenFor("developer", "demo", "new"); err != nil {
		t.Fatalf("256 revoked tokens and zero active must allow issuance: %v", err)
	}
}

// Seed realistic persisted records in one commit so boundary tests do not
// spend their time on hundreds of fsyncs. Issuance itself stays real above.
func seedCapTokens(t *testing.T, s *Store, active, revoked, expired int) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.data.Projects["demo"]
	now := s.now().UTC()
	for i := 0; i < active+revoked+expired; i++ {
		token := SessionToken{ScopeVersion: 1, Scopes: legacyScopesFixture, Kind: tokenProject, ID: rand.Text() + rand.Text(), TokenHash: digest(rand.Text()), OwnerID: "owner", IssuedAt: now}
		if i >= active && i < active+revoked {
			token.Revoked = true
		}
		if i >= active+revoked {
			token.Kind = ""
			token.Nickname = fmt.Sprintf("expired-%d", i)
			token.IssuedAt = now.Add(-time.Hour)
		}
		p.Tokens = append(p.Tokens, token)
	}
	if err := s.commitProject(p); err != nil {
		t.Fatal(err)
	}
}

func TestTokenCapMixedHistoryApprovalAndRestart(t *testing.T) {
	s := userStore(t)
	seedCapTokens(t, s, 0, 256, 256)
	raw, err := s.IssueProjectTokenFor("owner", "demo", "parent")
	if err != nil {
		t.Fatal(err)
	}
	code, err := s.BeginDeviceAuthorization("demo", raw, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "cap", InstanceID: "pc", RequestID: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApproveDeviceAuthorization("owner", code.UserCode, "expired-256", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IssueTokenFor("owner", "demo", "legacy-new", "owner"); err != nil {
		t.Fatal(err)
	}
	dir := s.root.Name()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(dir)
	if err != nil {
		t.Fatalf("mixed history must load: %v", err)
	}
	defer reopened.Close()
	if _, err := reopened.IssueProjectTokenFor("owner", "demo", "after-restart"); err != nil {
		t.Fatal(err)
	}
	if got := len(reopened.ProjectsFor("owner")[0].Tokens); got != 516 {
		t.Fatalf("retained history and new records = %d, want 516", got)
	}
}

func TestTokenCapConcurrentIssueAtActiveBoundary(t *testing.T) {
	s := userStore(t)
	seedCapTokens(t, s, 255, 256, 0)
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.IssueProjectTokenFor("owner", "demo", "race"); results <- err }()
	}
	wg.Wait()
	close(results)
	succeeded, capped := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrTokenCap) {
			capped++
		} else {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || capped != 11 {
		t.Fatalf("success=%d capped=%d", succeeded, capped)
	}
	if _, err := s.IssueTokenFor("owner", "demo", "blocked", "owner"); !errors.Is(err, ErrTokenCap) {
		t.Fatalf("legacy cap: %v", err)
	}
	if !validState(s.data) {
		t.Fatal("256 active plus history rejected by load validation")
	}
	s.mu.Lock()
	p := s.data.Projects["demo"]
	extra := p.Tokens[0]
	extra.ID = rand.Text() + rand.Text()
	extra.TokenHash = digest(rand.Text())
	p.Tokens = append(p.Tokens, extra)
	s.data.Projects["demo"] = p
	valid := validState(s.data)
	s.mu.Unlock()
	if valid {
		t.Fatal("257 active accepted by load validation")
	}
}

func TestTokenCapHTTPExplainsIssueAndApprovalLimit(t *testing.T) {
	f := newDeviceHTTPFixture(t)
	raw, err := f.store.IssueProjectTokenFor("owner", "demo", "parent")
	if err != nil {
		t.Fatal(err)
	}
	code, err := f.store.BeginDeviceAuthorization("demo", raw, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "cap", InstanceID: "pc", RequestID: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	seedCapTokens(t, f.store, 255, 256, 0)
	owner := loginPanelUser(t, f.h, "owner")
	for _, tc := range []struct {
		path   string
		form   url.Values
		status int
	}{
		{"/tokens/project", url.Values{"project": {"demo"}, "label": {"blocked"}}, http.StatusBadRequest},
		{"/auth/code", url.Values{"action": {"approve"}, "code": {code.UserCode}, "nickname": {"blocked"}}, http.StatusConflict},
	} {
		w := owner.call("POST", tc.path, tc.form)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), "256") || !strings.Contains(w.Body.String(), "오래된 세션을 해제") {
			t.Fatalf("%s: missing cap guidance, status=%d", tc.path, w.Code)
		}
	}
}

func TestTokenHistoryRetentionBoundaryAndSummary(t *testing.T) {
	f := newDeviceFixture(t)
	grant, req := f.approve(t, "owner", "retention", "reusable")
	g, _, ok := f.store.authorize("demo", grant.AccessToken)
	if !ok || g.Claim(req.identity()) != nil || g.Connected(req.identity()) != nil {
		t.Fatal("registration failed")
	}
	registered := f.now
	// Session expires seven days after last use; retain exactly 30 more days.
	f.now = registered.Add(7 * 24 * time.Hour)
	if f.store.Authorize("demo", grant.AccessToken) {
		t.Fatal("expired retained credential authenticated")
	}
	rows, err := f.store.reservedSessionGrants("demo", nil)
	if err != nil || len(rows) != 0 {
		t.Fatal("expired history reserves name")
	}
	f.now = f.now.Add(30*24*time.Hour - time.Nanosecond)
	if err := f.store.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	if len(f.store.Projects()[0].Tokens) != 2 {
		t.Fatal("history removed before 30 days")
	}
	f.now = f.now.Add(time.Nanosecond)
	if err := f.store.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	p := f.store.Projects()[0]
	if len(p.Tokens) != 1 || p.Tokens[0].Approvals != 1 || !p.Tokens[0].LastRegisteredAt.Equal(registered) {
		t.Fatal("sweep lost parent approval/registration summary")
	}
	if err := f.store.RevokeTokenFor("owner", "demo", p.Tokens[0].ID); err != nil {
		t.Fatal(err)
	}
	revoked := f.now
	f.now = revoked.Add(30*24*time.Hour - time.Nanosecond)
	if err := f.store.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	if len(f.store.Projects()[0].Tokens) != 1 {
		t.Fatal("revoked project removed early")
	}
	f.now = f.now.Add(time.Nanosecond)
	if err := f.store.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	if len(f.store.Projects()[0].Tokens) != 0 {
		t.Fatal("revoked project history not removed at 30 days")
	}
}

func TestTokenHistoryLegacyRevocationMigrationDoesNotExtendOnRestart(t *testing.T) {
	s := userStore(t)
	seedCapTokens(t, s, 0, 1, 0)
	old := s.data
	p := old.Projects["demo"]
	p.Tokens[0].RevokedAt = time.Time{}
	old.Projects["demo"] = p
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir() // Import into a fresh SQLite store.
	if err := os.WriteFile(filepath.Join(dir, "control.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC()
	reopened, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	stamp := reopened.Projects()[0].Tokens[0].RevokedAt
	if stamp.Before(before) || stamp.After(time.Now().UTC()) {
		t.Fatal("migration invented old revocation time")
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reopened.Projects()[0].Tokens[0].RevokedAt.Equal(stamp) {
		t.Fatal("restart extended retention")
	}
	reopened.now = func() time.Time { return stamp.Add(30 * 24 * time.Hour) }
	if err := reopened.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	if len(reopened.Projects()[0].Tokens) != 0 {
		t.Fatal("migrated revoked history not swept")
	}
}

func TestTokenHistoryLateSweepUsesActualExpiry(t *testing.T) {
	for _, kind := range []string{"legacy", "session-idle", "session-absolute"} {
		t.Run(kind, func(t *testing.T) {
			s := userStore(t)
			now := time.Now().UTC()
			s.now = func() time.Time { return now }
			if _, err := s.IssueTokenFor("owner", "demo", "history", "owner"); err != nil {
				t.Fatal(err)
			}
			token := s.data.Projects["demo"].Tokens[0]
			expires := now.Add(30 * time.Minute)
			if kind != "legacy" {
				token.Kind = tokenSession
				token.ParentID = rand.Text() + rand.Text()
				token.BoundIdentity = `["codex","history","pc"]`
				token.ExpiresAt = now.Add(30 * 24 * time.Hour)
				expires = now.Add(7 * 24 * time.Hour)
				if kind == "session-absolute" {
					token.LastUsedAt = now.Add(29 * 24 * time.Hour)
					expires = token.ExpiresAt
				}
			}
			p := s.data.Projects["demo"]
			p.Tokens[0] = token
			if err := s.commitProject(p); err != nil {
				t.Fatal(err)
			}
			now = expires.Add(30*24*time.Hour - time.Nanosecond)
			if err := s.SweepExpired(); err != nil {
				t.Fatal(err)
			}
			p = s.Projects()[0]
			if len(p.Tokens) != 1 || !p.Tokens[0].ExpiredAt.Equal(expires) {
				t.Fatal("late sweep reset expiry retention clock")
			}
			now = now.Add(time.Nanosecond)
			if err := s.SweepExpired(); err != nil {
				t.Fatal(err)
			}
			if len(s.Projects()[0].Tokens) != 0 {
				t.Fatal("late sweep missed 30-day expiry boundary")
			}
		})
	}
}
