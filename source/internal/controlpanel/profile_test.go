package controlpanel

import (
	"errors"
	"strings"
	"testing"
)

func TestProfileStoreUpdateAndRestart(t *testing.T) {
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
	before, _ := s.User("alice")
	if err = s.UpdateProfile("alice", "개발자 Alice", "alice@example.test"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.User("alice")
	if after.ID != before.ID || after.Role != before.Role || after.Revision != before.Revision || after.DisplayName != "개발자 Alice" || after.Email != "alice@example.test" {
		t.Fatal("profile update changed identity or lost fields")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	restarted, _ := s.User("alice")
	if restarted != after {
		t.Fatal("profile lost on restart")
	}
	if err = s.UpdateProfile("alice", "", ""); err != nil {
		t.Fatal("optional fields cannot be cleared", err)
	}
}

func TestProfileStoreValidationAndRollback(t *testing.T) {
	s := userStore(t)
	before, _ := s.User("developer")
	for _, tc := range []struct{ name, email string }{{"bad\nname", ""}, {strings.Repeat("가", 101), ""}, {"\xff", ""}, {"a", "not-an-email"}, {"a", "Name <a@example.test>"}, {"a", "a@example.test\r\n"}, {"a", strings.Repeat("x", 255)}} {
		if s.UpdateProfile("developer", tc.name, tc.email) == nil {
			t.Fatal("invalid profile accepted")
		}
	}
	s.write = func([]byte) error { return errors.New("unavailable") }
	if s.UpdateProfile("developer", "new name", "new@example.test") == nil {
		t.Fatal("failed profile write accepted")
	}
	after, _ := s.User("developer")
	if after != before {
		t.Fatal("failed profile update published")
	}
	if s.UpdateProfile("missing", "name", "") != ErrForbidden {
		t.Fatal("missing account updated")
	}
}

func TestProfileStorePasswordPolicyAndAtomicity(t *testing.T) {
	s := userStore(t)
	before := s.data.Users["developer"]
	for _, tc := range []struct{ current, next, confirm string }{{"wrong", testPassword, testPassword}, {testPassword, "short", "short"}, {testPassword, testPassword, "different"}, {testPassword, strings.Repeat("a", 1025), strings.Repeat("a", 1025)}} {
		if s.ChangeOwnPassword("developer", tc.current, tc.next, tc.confirm) == nil {
			t.Fatal("invalid password change accepted")
		}
	}
	next := "new profile password for tests"
	write := s.write
	s.write = func([]byte) error { return errors.New("unavailable") }
	if s.ChangeOwnPassword("developer", testPassword, next, next) == nil {
		t.Fatal("failed password save accepted")
	}
	if s.data.Users["developer"] != before {
		t.Fatal("failed password change published")
	}
	s.write = write
	if err := s.UpdateUser("admin", "developer", "user", false, ""); err != nil {
		t.Fatal(err)
	}
	if s.ChangeOwnPassword("developer", testPassword, next, next) != ErrForbidden || s.UpdateProfile("developer", "disabled", "") != ErrForbidden {
		t.Fatal("disabled account changed")
	}
}

func TestProfileStoreOwnAdminPasswordPersistsHashOnly(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	before, _ := s.User("admin")
	next := "new profile admin password for tests"
	if err = s.ChangeOwnPassword("admin", testPassword, next, next); err != nil {
		t.Fatal(err)
	}
	after, _ := s.User("admin")
	if after.Revision <= before.Revision {
		t.Fatal("password change retained revision")
	}
	raw, err := readStoredControlState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), next) || strings.Contains(string(raw), testPassword) {
		t.Fatal("plaintext password persisted")
	}
	if s.data.Users["admin"].Password != *s.data.Admin {
		t.Fatal("legacy verifier not synchronized")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Authenticate(testPassword) || !s.Authenticate(next) {
		t.Fatal("password restart failed")
	}
}
