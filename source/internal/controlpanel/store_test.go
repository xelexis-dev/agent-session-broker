package controlpanel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testPassword = "correct horse battery staple"

func TestStoreSetupIsSingleAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); results <- s.Setup(testPassword) }()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrAlreadySetup) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("setup successes = %d", successes)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.Ready() || !s.Authenticate(testPassword) || s.Authenticate("wrong password") {
		t.Fatal("persisted authentication failed")
	}
	if !errors.Is(s.Setup("another long password"), ErrAlreadySetup) {
		t.Fatal("setup reopened")
	}
	raw, err := readStoredControlState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), testPassword) {
		t.Fatal("plaintext password persisted")
	}
}

func TestSessionTokenLifecycle(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"private", "other"} {
		if err = s.SaveProject(name, "token", true, ""); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := s.IssueToken("private", "agent-one")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Authorize("private", raw) || s.Authorize("other", raw) || s.Authorize("private", "") {
		t.Fatal("project isolation failed")
	}
	g, _, ok := s.authorize("private", raw)
	if !ok || g.Nickname != "agent-one" || g.TokenID == "" {
		t.Fatal("missing grant")
	}
	if g.Claim("pc-one/codex/session") != nil {
		t.Fatal("same identity rejected")
	}
	// Repeating the same claim must remain idempotent.
	if g.Claim("pc-one/codex/session") != nil {
		t.Fatal("same identity rejected")
	}
	if g.Claim("pc-two/codex/session") == nil {
		t.Fatal("another PC stole token")
	}
	data, _ := readStoredControlState(dir)
	if strings.Contains(string(data), raw) {
		t.Fatal("raw token persisted")
	}
	listed, _ := json.Marshal(s.Projects())
	if strings.Contains(string(listed), raw) || strings.Contains(string(listed), digest(raw)) {
		t.Fatal("secret in listing")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	g, changes, ok := s.authorize("private", raw)
	if !ok || g.Claim("pc-two/codex/session") == nil {
		t.Fatal("binding lost on restart")
	}
	if err = s.RevokeToken("private", g.TokenID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	default:
		t.Fatal("revocation did not publish cancellation")
	}
	if s.Authorize("private", raw) || g.Claim("pc-one/codex/session") == nil {
		t.Fatal("revoked grant survived")
	}
	if _, err = s.IssueToken("private", "agent-one"); err != nil {
		t.Fatal("nickname not released", err)
	}
}
func TestIssueNicknameRaceAndValidation(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProject("private", "token", true, ""); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.IssueToken("private", "same-name"); results <- err }()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("reservations=%d", successes)
	}
	for _, name := range []string{"", " spaces", "two words", "tab\tname", "line\nname", strings.Repeat("a", 129)} {
		if _, err = s.IssueToken("private", name); err == nil {
			t.Fatalf("invalid nickname %q", name)
		}
	}
	if _, err = s.IssueToken("private", "123e4567-e89b-12d3-a456-426614174000"); err != nil {
		t.Fatal("UUID rejected", err)
	}
}
func TestGrantChecksChangedPolicy(t *testing.T) {
	for _, mode := range []string{"disabled"} {
		t.Run(mode, func(t *testing.T) {
			s, err := OpenSQLite(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err = s.Setup(testPassword); err != nil {
				t.Fatal(err)
			}
			if err = s.SaveProject("private", "token", true, ""); err != nil {
				t.Fatal(err)
			}
			raw, err := s.IssueToken("private", "agent")
			if err != nil {
				t.Fatal(err)
			}
			g, changes, ok := s.authorize("private", raw)
			if !ok {
				t.Fatal("unauthorized")
			}
			nextMode := "token"

			if err = s.SaveProject("private", nextMode, mode != "disabled", ""); err != nil {
				t.Fatal(err)
			}
			select {
			case <-changes:
			default:
				t.Fatal("policy did not cancel")
			}
			if g.Claim("pc") == nil {
				t.Fatal("stale grant accepted")
			}
		})
	}
}

func TestStoreFailedSaveDoesNotPublishPolicy(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProject("private", "token", true, ""); err != nil {
		t.Fatal(err)
	}
	testToken, err := s.IssueToken("private", "agent")
	if err != nil {
		t.Fatal(err)
	}
	s.write = func([]byte) error { return errors.New("disk unavailable") }
	if err := s.SaveProject("private", "token", false, ""); err == nil {
		t.Fatal("failed write reported success")
	}
	if s.Authorize("private", "") || !s.Authorize("private", testToken) {
		t.Fatal("failed policy was published")
	}
}

func TestStoreCorruptionDoesNotReopenSetup(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "control.json"), []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenSQLite(dir); err == nil {
		s.Close()
		t.Fatal("corrupt state reopened setup")
	}
}

func TestStoreExclusiveOwnership(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := OpenSQLite(dir); err == nil {
		other.Close()
		t.Fatal("two writers acquired store")
	}
	s.Close()
	other, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
}

func TestImportProjectsPreservesPolicyAndIsAtomic(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.ImportProjects([]string{"before-setup"}) == nil {
		t.Fatal("import before setup")
	}
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProject("existing", "token", false, ""); err != nil {
		t.Fatal(err)
	}
	if err = s.ImportProjects([]string{"existing", "new", "new"}); err != nil {
		t.Fatal(err)
	}
	if s.Authorize("existing", "") || s.Authorize("new", "") || s.data.Projects["new"].Mode != "token" {
		t.Fatal("import policy incorrect")
	}
	if err = s.ImportProjects([]string{"partial", "../bad"}); err == nil {
		t.Fatal("invalid import accepted")
	}
	if len(s.Projects()) != 2 {
		t.Fatal("partial import published")
	}
	s.write = func([]byte) error { return errors.New("unavailable") }
	if s.ImportProjects([]string{"failed"}) == nil || len(s.Projects()) != 2 {
		t.Fatal("failed import published")
	}
}

func TestStaleGrantCannotReturnAfterPolicyToggle(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProject("private", "token", true, ""); err != nil {
		t.Fatal(err)
	}
	raw, err := s.IssueToken("private", "agent")
	if err != nil {
		t.Fatal(err)
	}
	g, _, _ := s.authorize("private", raw)
	if err = s.SaveProject("private", "token", false, ""); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProject("private", "token", true, ""); err != nil {
		t.Fatal(err)
	}
	if g.Claim("pc/session") == nil {
		t.Fatal("stale grant revived after policy toggle")
	}
	fresh, _, ok := s.authorize("private", raw)
	if !ok || fresh.Claim("pc/session") != nil {
		t.Fatal("fresh grant rejected")
	}
}

func TestClaimRaceAndFailedPersistence(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProject("private", "token", true, ""); err != nil {
		t.Fatal(err)
	}
	raw, err := s.IssueToken("private", "agent")
	if err != nil {
		t.Fatal(err)
	}
	g, changes, _ := s.authorize("private", raw)
	write := s.write
	s.write = func([]byte) error { return errors.New("unavailable") }
	if g.Claim("pc-one") == nil {
		t.Fatal("claim accepted without persistence")
	}
	if s.RevokeToken("private", g.TokenID) == nil {
		t.Fatal("revocation ignored disk error")
	}
	if _, err = s.IssueToken("private", "new-agent"); err == nil {
		t.Fatal("issued without persistence")
	}
	select {
	case <-changes:
		t.Fatal("failed mutation cancelled policy")
	default:
	}
	if !s.Authorize("private", raw) || s.Projects()[0].Tokens[0].BoundIdentity != "" {
		t.Fatal("failed change published")
	}
	s.write = write
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, identity := range []string{"pc-one", "pc-two"} {
		wg.Add(1)
		go func(identity string) { defer wg.Done(); results <- g.Claim(identity) }(identity)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("bound %d identities", successes)
	}
}

func TestImportCapAndTokenProjectCancellation(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 129)
	for i := range names {
		names[i] = fmt.Sprintf("project-%d", i)
	}
	if s.ImportProjects(names) == nil || len(s.Projects()) != 0 {
		t.Fatal("oversize import partially published")
	}
	if err = s.ImportProjects(names[:128]); err != nil {
		t.Fatal(err)
	}
	raw, err := s.IssueToken("project-0", "agent")
	if err != nil {
		t.Fatal(err)
	}
	_, changes, ok := s.authorize("project-0", raw)
	if !ok {
		t.Fatal("token project rejected")
	}
	if err = s.SaveProject("project-0", "token", false, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	default:
		t.Fatal("token request not cancelled on disable")
	}
	if s.Authorize("project-0", "") {
		t.Fatal("token mode without credentials is not closed")
	}
}

func TestGrantCheckNeverBindsAndRejectsInvalidatedIdentity(t *testing.T) {
	for _, change := range []string{"revoke", "disable-enable"} {
		t.Run(change, func(t *testing.T) {
			s, err := OpenSQLite(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err = s.Setup(testPassword); err != nil {
				t.Fatal(err)
			}
			if err = s.SaveProject("private", "token", true, ""); err != nil {
				t.Fatal(err)
			}
			raw, err := s.IssueToken("private", "agent")
			if err != nil {
				t.Fatal(err)
			}
			g, _, ok := s.authorize("private", raw)
			if !ok {
				t.Fatal("grant rejected")
			}
			if g.Check("pc-one") == nil || g.Check("") == nil {
				t.Fatal("unbound check authorized")
			}
			if s.Projects()[0].Tokens[0].BoundIdentity != "" {
				t.Fatal("Check bound an identity")
			}
			if err = g.Claim("pc-two"); err != nil {
				t.Fatal("unbound Check reserved identity", err)
			}
			if err = g.Check("pc-two"); err != nil {
				t.Fatal("bound identity denied", err)
			}
			if g.Check("pc-one") == nil || g.Check("") == nil {
				t.Fatal("other identity authorized")
			}
			switch change {
			case "revoke":
				err = s.RevokeToken("private", g.TokenID)
			case "disable-enable":
				err = s.SaveProject("private", "token", false, "")
				if err == nil {
					err = s.SaveProject("private", "token", true, "")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if g.Check("pc-two") == nil {
				t.Fatal("invalidated check accepted")
			}
		})
	}
}

func TestIssueTokenRejectsBrokerReservedNicknames(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProject("private", "token", true, ""); err != nil {
		t.Fatal(err)
	}
	for _, nickname := range []string{"name:agent", "session:123e4567-e89b-12d3-a456-426614174000", "registration:agent", "name:", "session:", "registration:", "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "22222222222222222222222222"} {
		if raw, err := s.IssueToken("private", nickname); err == nil || raw != "" {
			t.Errorf("reserved nickname %q issued a token", nickname)
		}
	}
	if len(s.Projects()[0].Tokens) != 0 {
		t.Fatal("reserved nickname persisted a reservation")
	}
	raw, err := s.IssueToken("private", "123e4567-e89b-12d3-a456-426614174000")
	if err != nil || !s.Authorize("private", raw) {
		t.Fatal("ordinary UUID nickname rejected", err)
	}
}
