package controlpanel

import (
	"bytes"
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 0.2.0 and 0.2.1 wrote this v1 JSON shape. Opening SQLite directly must
// perform both the storage import and account migration without an interim run.
func TestSQLiteDirectLegacyV1Upgrade(t *testing.T) {
	root := t.TempDir()
	old, err := OpenSQLite(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{
		"version": 1, "admin": old.data.Admin,
		"projects": map[string]any{"demo": map[string]any{
			"name": "demo", "mode": "open", "enabled": true,
		}},
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "control.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenSQLite(root)
	if err != nil {
		t.Fatal(err)
	}
	if store.data.Version != controlVersion || store.ProjectRole("admin", "demo") != "owner" {
		t.Fatal("direct v1 import lost account migration")
	}
	if _, ok := store.AuthenticateUser("admin", testPassword); !ok {
		t.Fatal("direct v1 import lost administrator password")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(root, "control.json"))
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("legacy JSON changed during direct migration")
	}
	store, err = OpenSQLite(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.data.Version != controlVersion || store.ProjectRole("admin", "demo") != "owner" {
		t.Fatal("direct upgrade did not persist across restart")
	}
}

func TestSQLiteCorruptStateCannotReopenSetup(t *testing.T) {
	for _, payload := range []string{"null", "{}", `{"version":2,"admin":null,"projects":null}`, "missing"} {
		t.Run(payload, func(t *testing.T) {
			root := t.TempDir()
			s, err := OpenSQLite(root)
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			db, err := sqlitestore.Open(root, "control.db")
			if err != nil {
				t.Fatal(err)
			}
			if payload == "missing" {
				err = db.Delete("state")
			} else {
				err = db.Put("state", []byte(payload))
			}
			if err != nil {
				t.Fatal(err)
			}
			db.Close()
			if s, err := OpenSQLite(root); err == nil {
				s.Close()
				t.Fatal("corrupt state reopened setup")
			}
		})
	}
}

func TestSQLiteMigratesControlWithoutChangingLegacyFile(t *testing.T) {
	root := t.TempDir()
	old, err := OpenSQLite(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = old.Setup("sqlite-fixture-password"); err != nil {
		t.Fatal(err)
	}
	if err = old.CreateUser("admin", "alice", "alice-fixture-password", "user"); err != nil {
		t.Fatal(err)
	}
	if err = old.CreateProjectFor("alice", "project"); err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(old.data)
	old.Close()
	root = t.TempDir()
	if err == nil {
		err = os.WriteFile(filepath.Join(root, "control.json"), before, 0600)
	}
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenSQLite(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.AuthenticateUser("alice", "alice-fixture-password"); !ok {
		t.Fatal("lost account")
	}
	if store.ProjectRole("alice", "project") != "owner" {
		t.Fatal("lost owner")
	}
	if err = store.CreateUser("admin", "bob", "bob-fixture-password", "user"); err != nil {
		t.Fatal(err)
	}
	store.Close()
	after, _ := os.ReadFile(filepath.Join(root, "control.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("legacy backup changed")
	}
	db, _ := os.ReadFile(filepath.Join(root, "control.db"))
	if !bytes.HasPrefix(db, []byte("SQLite format 3\x00")) {
		t.Fatal("not a SQLite database")
	}
	// An old JSON backup cannot overwrite the primary database on restart.
	store, err = OpenSQLite(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, ok := store.User("bob"); !ok {
		t.Fatal("SQLite change lost on restart")
	}
}

func TestSQLiteSetupOnlyStoresDatabaseAndRejectsCorruption(t *testing.T) {
	root := t.TempDir()
	store, err := OpenSQLite(root)
	if err != nil {
		t.Fatal(err)
	}
	if store.Ready() {
		t.Fatal("unexpected initial administrator")
	}
	if err = store.Setup("sqlite-new-fixture-password"); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if _, err = os.Stat(filepath.Join(root, "control.json")); !os.IsNotExist(err) {
		t.Fatal("new setup wrote JSON")
	}
	if err = os.WriteFile(filepath.Join(root, "control.db"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if reopened, err := OpenSQLite(root); err == nil {
		reopened.Close()
		t.Fatal("corrupt DB reopened as setup")
	}
}

func TestSQLiteControlWriteFailureKeepsState(t *testing.T) {
	root := t.TempDir()
	store, err := OpenSQLite(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Setup("sqlite-fixture-password"); err != nil {
		t.Fatal(err)
	}
	store.write = func([]byte) error { return os.ErrPermission }
	if err = store.CreateProject("lost"); err == nil {
		t.Fatal("write failure ignored")
	}
	if len(store.Projects()) != 0 {
		t.Fatal("failed state published")
	}
	store.Close()
	store, err = OpenSQLite(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if len(store.Projects()) != 0 {
		t.Fatal("failed state persisted")
	}
}

func TestSQLiteProfileAndPasswordPersistTogether(t *testing.T) {
	root := t.TempDir()
	s, err := OpenSQLite(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Setup("sqlite-profile-password"); err != nil {
		t.Fatal(err)
	}
	if err = s.UpdateProfile("admin", "관리자 이름", "admin@example.test"); err != nil {
		t.Fatal(err)
	}
	u, _ := s.User("admin")
	if err = s.ChangeOwnPassword("admin", "sqlite-profile-password", "sqlite-profile-updated", "sqlite-profile-updated"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenSQLite(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after, ok := s.AuthenticateUser("admin", "sqlite-profile-updated")
	if !ok || after.DisplayName != "관리자 이름" || after.Email != "admin@example.test" || after.Revision != u.Revision+1 {
		t.Fatal("SQLite lost profile/password revision")
	}
	if _, ok = s.AuthenticateUser("admin", "sqlite-profile-password"); ok {
		t.Fatal("old password still accepted")
	}
}

func TestSQLiteImportsRevokedHistoryAndPersistsTimestamp(t *testing.T) {
	seed := userStore(t)
	seedCapTokens(t, seed, 0, 1, 0)
	legacy := seed.data
	p := legacy.Projects["demo"]
	p.Tokens[0].RevokedAt = time.Time{}
	legacy.Projects["demo"] = p
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal("legacy fixture marshal failed")
	}
	dir := t.TempDir()
	if os.WriteFile(filepath.Join(dir, "control.json"), raw, 0600) != nil {
		t.Fatal("legacy fixture write failed")
	}
	before := time.Now().UTC()
	store, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal("legacy fixture import failed")
	}
	defer store.Close()
	store.mu.RLock()
	stamp := store.data.Projects["demo"].Tokens[0].RevokedAt
	store.mu.RUnlock()
	if stamp.IsZero() || stamp.Before(before) || stamp.After(time.Now().UTC()) {
		t.Fatal("SQLite import did not start revoked-token retention")
	}
	if store.Close() != nil {
		t.Fatal("import close failed")
	}
	reopened, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal("SQLite migrated DB reopen failed")
	}
	defer reopened.Close()
	reopened.mu.RLock()
	reloaded := reopened.data.Projects["demo"].Tokens[0].RevokedAt
	reopened.mu.RUnlock()
	if !reloaded.Equal(stamp) {
		t.Fatal("SQLite reopen changed revocation timestamp")
	}
	after, err := os.ReadFile(filepath.Join(dir, "control.json"))
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("import rewrote legacy JSON")
	}
}

func TestSQLiteCurrentDatabaseReopensUnchanged(t *testing.T) {
	s := userStore(t)
	dir := s.root.Name()
	if s.Close() != nil {
		t.Fatal("fixture close failed")
	}
	before, err := os.ReadFile(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal("fixture DB read failed")
	}
	reopened, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal("current database did not reopen")
	}
	if reopened.data.Version != controlVersion {
		t.Fatal("current schema version changed")
	}
	if reopened.Close() != nil {
		t.Fatal("reopened DB close failed")
	}
	after, err := os.ReadFile(filepath.Join(dir, "control.db"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("reopening current SQLite DB rewrote it")
	}
}

func TestSQLiteImportKeepsStrictJSONAndSizeLimits(t *testing.T) {
	seed := userStore(t)
	raw, err := json.Marshal(seed.data)
	if err != nil {
		t.Fatal("fixture encoding failed")
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		t.Fatal("fixture parsing failed")
	}
	obj["unknownField"] = true
	unknown, err := json.Marshal(obj)
	if err != nil {
		t.Fatal("unknown fixture encoding failed")
	}
	delete(obj, "unknownField")
	obj["version"] = 3
	legacy, err := json.Marshal(obj)
	if err != nil {
		t.Fatal("legacy fixture encoding failed")
	}
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"unknown-field", unknown}, {"trailing-value", append(append([]byte{}, raw...), []byte("{}")...)},
		{"current-size", append(append([]byte{}, raw...), bytes.Repeat([]byte(" "), controlMaxBytes-len(raw)+1)...)},
		{"legacy-size", append(legacy, bytes.Repeat([]byte(" "), legacyControlMaxBytes-len(legacy)+1)...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if os.WriteFile(filepath.Join(dir, "control.json"), tc.raw, 0600) != nil {
				t.Fatal("import fixture write failed")
			}
			if s, err := OpenSQLite(dir); err == nil {
				s.Close()
				t.Fatal("invalid legacy import accepted")
			}
		})
	}
}
