package controlpanel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewLastUsageSurvivesSessionExpiry(t *testing.T) {
	f := newDeviceFixture(t)
	credential, _ := f.approve(t, "owner", "review-history", "review-agent")
	f.now = f.now.Add(2 * time.Hour)
	if !f.store.Authorize("demo", credential.AccessToken) {
		t.Fatal("authorization failed")
	}
	want := f.now
	f.now = f.now.Add(8 * 24 * time.Hour)
	if err := f.store.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	views := projectTokenViews(f.store.ProjectsFor("owner")[0].Tokens, nil, func(SessionToken) bool { return true })
	if len(views) != 1 || !views[0].LastUsed.Equal(want) {
		t.Fatalf("last usage moved backwards after grant expiry: got %v, want %v", views[0].LastUsed, want)
	}
}

func TestReviewExistingRegisteredTokenHistoryAfterUpgrade(t *testing.T) {
	f := newDeviceFixture(t)
	credential, req := f.approve(t, "owner", "review-upgrade", "upgrade-agent")
	grant, _, ok := f.store.authorize("demo", credential.AccessToken)
	if !ok {
		t.Fatal("authorization failed")
	}
	if err := grant.Connected(req.identity()); err != nil {
		t.Fatal(err)
	}
	// Persist the fields older releases stored: session grants carry history,
	// but their parent has none of the newly introduced aggregate values.
	f.store.mu.Lock()
	old := f.store.data
	p := old.Projects["demo"]
	p.Tokens = append([]SessionToken(nil), p.Tokens...)
	for i := range p.Tokens {
		if p.Tokens[i].Kind == tokenProject {
			p.Tokens[i].Approvals = 0
			p.Tokens[i].LastRegisteredAt = time.Time{}
			p.Tokens[i].LastUsedAt = time.Time{}
		}
	}
	old.Projects = map[string]Project{"demo": p}
	data, err := json.Marshal(old)
	f.store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "control.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	views := projectTokenViews(s.ProjectsFor("owner")[0].Tokens, nil, func(SessionToken) bool { return true })
	if len(views) != 1 || views[0].State != "registered" || views[0].Approvals != 1 {
		t.Fatalf("upgrade loses existing registration: state=%s approvals=%d", views[0].State, views[0].Approvals)
	}
}
