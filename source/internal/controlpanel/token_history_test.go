package controlpanel

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

// legacyProjectTokenState builds 0.7.0-shaped control state: a project token
// whose session grants carry history while the parent has no aggregates.
func legacyProjectTokenState(t *testing.T, now time.Time) ([]byte, string) {
	t.Helper()
	seed, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()
	if err = seed.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = seed.CreateProject("demo"); err != nil {
		t.Fatal(err)
	}
	seed.now = func() time.Time { return now }
	raw, err := seed.IssueProjectTokenFor("admin", "demo", "legacy")
	if err != nil {
		t.Fatal(err)
	}
	var parentID string
	for _, token := range seed.Projects()[0].Tokens {
		parentID = token.ID
	}
	for i, req := range []DeviceRequest{
		{ClientID: "asb-client-v1", Harness: "claude", SessionID: "conversation-1", InstanceID: "pc-1", RequestID: "request-1"},
		{ClientID: "asb-client-v1", Harness: "codex", SessionID: "conversation-2", InstanceID: "pc-2", RequestID: "request-2"},
	} {
		issued, err := seed.BeginDeviceAuthorization("demo", raw, req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = seed.ApproveDeviceAuthorization("admin", issued.UserCode, "agent-"+req.Harness, false); err != nil {
			t.Fatal(err)
		}
		seed.mu.Lock()
		seed.devicePoll = nil
		seed.mu.Unlock()
		granted, err := seed.ExchangeDeviceToken("demo", raw, issued.DeviceCode, "asb-client-v1", false)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			grant, _, ok := seed.authorize("demo", granted.AccessToken)
			if !ok {
				t.Fatal("grant unauthorized")
			}
			if err = grant.Connected(req.identity()); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed.mu.Lock()
	defer seed.mu.Unlock()
	legacy := seed.data
	legacy.Projects = maps.Clone(seed.data.Projects)
	p := legacy.Projects["demo"]
	p.Tokens = append([]SessionToken(nil), p.Tokens...)
	for i := range p.Tokens {
		if p.Tokens[i].Kind == tokenProject {
			p.Tokens[i].Approvals = 0
			p.Tokens[i].LastRegisteredAt = time.Time{}
			p.Tokens[i].LastUsedAt = time.Time{}
		}
	}
	legacy.Projects["demo"] = p
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	return data, parentID
}

func parentTokenOf(t *testing.T, s *Store, id string) SessionToken {
	t.Helper()
	for _, token := range s.Projects()[0].Tokens {
		if token.ID == id {
			return token
		}
	}
	t.Fatal("parent token missing")
	return SessionToken{}
}

func TestTokenHistoryBackfillRunsOnceAndSurvivesReload(t *testing.T) {
	for _, storage := range []string{"json-import", "sqlite"} {
		t.Run(storage, func(t *testing.T) {
			now := time.Date(2026, 9, 16, 3, 0, 0, 0, time.UTC)
			data, parentID := legacyProjectTokenState(t, now)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "control.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			opener := OpenSQLite
			if storage == "sqlite" {
				db, err := sqlitestore.Open(dir, "control.db")
				if err != nil {
					t.Fatal(err)
				}
				if err = db.Initialize(map[string][]byte{"state": data}); err != nil {
					t.Fatal(err)
				}
				if err = db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			for round := range 2 {
				s, err := opener(dir)
				if err != nil {
					t.Fatal(err)
				}
				// Pin the clock to the fixture so its session grants never idle-expire.
				s.now = func() time.Time { return now }
				parent := parentTokenOf(t, s, parentID)
				// Two grants were approved, one registered, one exchanged and used.
				if parent.Approvals != 2 || !parent.LastRegisteredAt.Equal(now) || !parent.LastUsedAt.Equal(now) {
					t.Fatalf("round %d backfill = approvals %d registered %v used %v", round, parent.Approvals, parent.LastRegisteredAt, parent.LastUsedAt)
				}
				views := projectTokenViews(s.Projects()[0].Tokens, nil, func(SessionToken) bool { return true })
				if len(views) != 1 || views[0].State != "registered" {
					t.Fatalf("round %d state = %+v", round, views)
				}
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			s, err := opener(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			s.now = func() time.Time { return now }
			// A new approval counts on top of the backfilled history, once.
			raw, err := s.IssueProjectTokenFor("admin", "demo", "fresh")
			if err != nil {
				t.Fatal(err)
			}
			_ = raw
			s.mu.Lock()
			p := s.data.Projects["demo"]
			tokens := p.Tokens
			s.mu.Unlock()
			if len(tokens) != 4 {
				t.Fatalf("tokens = %d", len(tokens))
			}
			if parent := parentTokenOf(t, s, parentID); parent.Approvals != 2 {
				t.Fatalf("issuing another token changed backfilled approvals: %d", parent.Approvals)
			}
		})
	}
}

func TestTokenHistoryBackfillFailureKeepsState(t *testing.T) {
	now := time.Date(2026, 9, 16, 3, 0, 0, 0, time.UTC)
	data, _ := legacyProjectTokenState(t, now)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "control.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	// Reset the aggregates in memory so the backfill has work to do again.
	p := s.data.Projects["demo"]
	p.Tokens = append([]SessionToken(nil), p.Tokens...)
	for i := range p.Tokens {
		if p.Tokens[i].Kind == tokenProject {
			p.Tokens[i].Approvals = 0
			p.Tokens[i].LastRegisteredAt = time.Time{}
		}
	}
	s.data.Projects["demo"] = p
	before, err := json.Marshal(s.data)
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	s.write = func([]byte) error { writes++; return errors.New("injected backfill write failure") }
	if err := s.backfillTokenHistory(); !errors.Is(err, ErrStorage) {
		t.Fatalf("backfill = %v", err)
	}
	after, err := json.Marshal(s.data)
	if err != nil || !bytes.Equal(before, after) || writes != 1 {
		t.Error("failed backfill published partial state")
	}
}

func TestSessionUseStampsParentProjectTokenAcrossReload(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateProject("demo"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 16, 4, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	raw, err := s.IssueProjectTokenFor("admin", "demo", "home")
	if err != nil {
		t.Fatal(err)
	}
	req := DeviceRequest{ClientID: "asb-client-v1", Harness: "claude", SessionID: "conversation-1", InstanceID: "pc-1", RequestID: "request-1"}
	issued, err := s.BeginDeviceAuthorization("demo", raw, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApproveDeviceAuthorization("admin", issued.UserCode, "home-claude-1", false); err != nil {
		t.Fatal(err)
	}
	granted, err := s.ExchangeDeviceToken("demo", raw, issued.DeviceCode, "asb-client-v1", false)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Hour)
	if !s.Authorize("demo", granted.AccessToken) {
		t.Fatal("credential rejected")
	}
	if parent := projectTokenOf(t, s); !parent.LastUsedAt.Equal(now) {
		t.Fatalf("session use did not stamp the parent: %v", parent.LastUsedAt)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if parent := projectTokenOf(t, s); !parent.LastUsedAt.Equal(now) {
		t.Fatalf("parent last use lost on reload: %v", parent.LastUsedAt)
	}
}
