package controlpanel

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func userStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"owner", "developer", "viewer", "outsider"} {
		if err = s.CreateUser("admin", id, testPassword, "user"); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.CreateProjectFor("owner", "demo"); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProjectFor("owner", "demo", "token", true); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"developer", "viewer"} {
		if err = s.SetProjectMember("owner", "demo", id, id); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestUserPasswordAndAccountPersistenceKeepSecretsPrivate(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateUser("admin", "alice", testPassword, "user"); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateProjectFor("alice", "alice-project"); err != nil {
		t.Fatal(err)
	}
	before, _ := s.User("alice")
	newPassword := "replacement account test password"
	if err = s.UpdateUser("admin", "alice", "user", true, newPassword); err != nil {
		t.Fatal(err)
	}
	after, _ := s.User("alice")
	if after.Revision <= before.Revision {
		t.Fatal("password update retained revision")
	}
	if _, ok := s.AuthenticateUser("alice", testPassword); ok {
		t.Fatal("old password accepted")
	}
	if _, ok := s.AuthenticateUser("alice", newPassword); !ok {
		t.Fatal("new password rejected")
	}
	users, err := s.Users("admin")
	if err != nil {
		t.Fatal(err)
	}
	public, _ := json.Marshal(users)
	private, _ := readStoredControlState(dir)
	if strings.Contains(string(private), testPassword) || strings.Contains(string(private), newPassword) || strings.Contains(string(public), "hash") || strings.Contains(string(public), "salt") {
		t.Fatal("account secrets exposed")
	}
	if _, err = os.Stat(filepath.Join(dir, "admin-password.txt")); !os.IsNotExist(err) {
		t.Fatal("plaintext bootstrap file exists")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if u, ok := s.AuthenticateUser("alice", newPassword); !ok || u != after {
		t.Fatal("account restart lost identity or revision")
	}
	if s.ProjectRole("alice", "alice-project") != "owner" {
		t.Fatal("creator membership lost")
	}
	if err = s.UpdateUser("admin", "admin", "admin", true, newPassword); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.Authenticate(newPassword) || s.Authenticate(testPassword) {
		t.Fatal("admin password update lost")
	}
}

func TestProjectRoleTokenViewsAndRevocationIsolation(t *testing.T) {
	s := userStore(t)
	mine, err := s.IssueTokenFor("developer", "demo", "mine", "developer")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.IssueTokenFor("owner", "demo", "other", "owner")
	if err != nil {
		t.Fatal(err)
	}
	all := s.ProjectsFor("owner")[0]
	if len(s.ProjectsFor("developer")[0].Tokens) != 1 || len(s.ProjectsFor("viewer")[0].Tokens) != 0 || len(all.Tokens) != 2 {
		t.Fatal("token visibility escaped role")
	}
	if s.RevokeTokenFor("developer", "demo", all.Tokens[1].ID) != ErrForbidden || s.RevokeTokenFor("viewer", "demo", all.Tokens[0].ID) != ErrForbidden {
		t.Fatal("revocation ownership bypass")
	}
	all.Members["developer"] = "owner"
	all.Tokens[0].Revoked = true
	if s.ProjectRole("developer", "demo") != "developer" || !s.Authorize("demo", mine) {
		t.Fatal("returned DTO mutated store")
	}
	if err = s.RevokeTokenFor("developer", "demo", all.Tokens[0].ID); err != nil {
		t.Fatal(err)
	}
	if s.Authorize("demo", mine) || !s.Authorize("demo", other) {
		t.Fatal("revoke affected wrong token")
	}
}

func TestProjectRoleConcurrentOwnerLossIsRejected(t *testing.T) {
	s := userStore(t)
	if err := s.SetProjectMember("owner", "demo", "developer", "owner"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); results <- s.UpdateUser("admin", "owner", "user", false, "") }()
	go func() { defer wg.Done(); results <- s.SetProjectMember("admin", "demo", "developer", "viewer") }()
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("owner-loss mutations succeeded %d times", success)
	}
	p := s.Projects()[0]
	owners := 0
	for id, role := range p.Members {
		u, _ := s.User(id)
		if role == "owner" && u.Enabled {
			owners++
		}
	}
	if owners != 1 {
		t.Fatal("last active owner lost")
	}
}

func TestUserAuthenticationRevisionAndDisable(t *testing.T) {
	s := userStore(t)
	before, ok := s.AuthenticateUser("developer", testPassword)
	if !ok {
		t.Fatal("login failed")
	}
	raw, err := s.IssueTokenFor("developer", "demo", "dev", "developer")
	if err != nil {
		t.Fatal(err)
	}
	g, changes, _ := s.authorize("demo", raw)
	if err = g.Claim("codex:session:client"); err != nil {
		t.Fatal(err)
	}
	if err = s.UpdateUser("admin", "developer", "user", false, ""); err != nil {
		t.Fatal(err)
	}
	after, _ := s.User("developer")
	if after.Revision <= before.Revision {
		t.Fatal("revision unchanged")
	}
	if _, ok = s.AuthenticateUser("developer", testPassword); ok || s.Authorize("demo", raw) || g.Check("codex:session:client") == nil {
		t.Fatal("disabled credentials survived")
	}
	select {
	case <-changes:
	default:
		t.Fatal("disable did not cancel")
	}
	if s.UpdateUser("admin", "admin", "user", true, "") == nil || s.UpdateUser("admin", "owner", "user", false, "") == nil {
		t.Fatal("protected account changed")
	}
	if _, err = s.Users("owner"); err == nil {
		t.Fatal("project owner became system admin")
	}
}

func TestProjectRoleGuardsAndLastOwner(t *testing.T) {
	s := userStore(t)
	if s.ProjectRole("owner", "demo") != "owner" || len(s.ProjectsFor("outsider")) != 0 {
		t.Fatal("membership isolation failed")
	}
	for _, actor := range []string{"outsider", "viewer", "developer"} {
		if s.SaveProjectFor(actor, "demo", "token", true) == nil || s.SetProjectMember(actor, "demo", "outsider", "owner") == nil {
			t.Fatal("non-owner changed policy")
		}
	}
	if s.SetProjectMember("owner", "demo", "owner", "") == nil || s.SetProjectMember("owner", "demo", "owner", "developer") == nil {
		t.Fatal("last owner lost")
	}
	if err := s.SetProjectMember("owner", "demo", "developer", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectMember("owner", "demo", "owner", "developer"); err != nil {
		t.Fatal(err)
	}
	if s.SetProjectMember("admin", "demo", "developer", "") == nil {
		t.Fatal("admin removed last owner")
	}
	if _, err := s.IssueTokenFor("viewer", "demo", "v", "viewer"); err == nil {
		t.Fatal("viewer issued token")
	}
	if _, err := s.IssueTokenFor("owner", "demo", "spoof", "developer"); err == nil {
		t.Fatal("developer spoofed owner")
	}
	if _, err := s.IssueTokenFor("developer", "demo", "outsider", "outsider"); err == nil {
		t.Fatal("owner issued outsider token")
	}
	raw, err := s.IssueTokenFor("owner", "demo", "mine", "owner")
	if err != nil {
		t.Fatal(err)
	}
	g, _, _ := s.authorize("demo", raw)
	if err = g.Claim("codex:s:c"); err != nil {
		t.Fatal(err)
	}
	if !s.CanManageSession("owner", "demo", "codex:s:c") || s.CanManageSession("viewer", "demo", "codex:s:c") {
		t.Fatal("session ownership failed")
	}
	if err = s.SetProjectMember("developer", "demo", "owner", "viewer"); err != nil {
		t.Fatal(err)
	}
	if s.Authorize("demo", raw) {
		t.Fatal("downgrade kept token")
	}
}

func TestUserFailedMutationDoesNotPublish(t *testing.T) {
	s := userStore(t)
	before, _ := s.User("developer")
	s.write = func([]byte) error { return errors.New("disk unavailable") }
	if s.UpdateUser("admin", "developer", "admin", false, "") == nil || s.SetProjectMember("owner", "demo", "developer", "owner") == nil {
		t.Fatal("failed save accepted")
	}
	after, _ := s.User("developer")
	if after != before || s.ProjectRole("developer", "demo") != "developer" {
		t.Fatal("failed state published")
	}
}
