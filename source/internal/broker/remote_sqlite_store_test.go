package broker

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

func TestSQLiteRegistrationsMigrateAndDoNotRevive(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	old, err := newRemoteService(root, false)
	if err != nil {
		t.Fatal(err)
	}
	request := durableRegister(1, "persisted", "client-sqlite", durableSecretA, "lease-sqlite", "request-sqlite")
	registered, err := old.RegisterSession(ctx, request)
	if err != nil || registered.Code != "" {
		t.Fatalf("legacy register: %+v %v", registered, err)
	}
	old.Close()
	st, err := NewSQLitePersistentRemoteService(root)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := st.GetSession(ctx, "persisted")
	if err != nil || restored.Status != StatusMissing || restored.RegistrationID != registered.Session.RegistrationID {
		t.Fatalf("restore: %+v %v", restored, err)
	}
	backup := filepath.Join(root, registered.Session.RegistrationID+".json")
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("legacy backup removed: %v", err)
	}
	if err := st.store.remove(registered.Session.RegistrationID); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = NewSQLitePersistentRemoteService(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.GetSession(ctx, "persisted"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("legacy revived: %v", err)
	}
}

func TestSQLiteRegistrationSaveFailureDoesNotPublishSession(t *testing.T) {
	service, err := NewSQLitePersistentRemoteService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.store.database.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := service.RegisterSession(context.Background(), durableRegister(1, "failed", "client-sqlite", durableSecretA, "lease", "request"))
	if err == nil && result.Code == "" {
		t.Fatal("registration succeeded despite closed database")
	}
	records, err := service.ListSessions(context.Background())
	if err != nil || len(records) != 0 {
		t.Fatalf("failed write published session: %v %v", records, err)
	}
}

func TestSQLiteRegistrationCorruptPayloadIsQuarantined(t *testing.T) {
	root := t.TempDir()
	service, err := NewSQLitePersistentRemoteService(root)
	if err != nil {
		t.Fatal(err)
	}
	id := "AAAAAAAAAAAAAAAAAAAAAAAAAA"
	if err := service.store.database.Put(id, []byte("broken record")); err != nil {
		t.Fatal(err)
	}
	service.Close()
	service, err = NewSQLitePersistentRemoteService(root)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	records, err := service.store.loadAll(time.Now())
	if err != nil || len(records) != 0 {
		t.Fatalf("invalid row restored: %v %v", records, err)
	}
	if _, err := service.store.database.Get(id); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("invalid row still active: %v", err)
	}
	backups, err := filepath.Glob(filepath.Join(root, id+".json.corrupt-*"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("quarantine backup: %v %v", backups, err)
	}
}

func TestSQLiteDetachedExpiryRemainsDeletedAfterRestart(t *testing.T) {
	root := t.TempDir()
	service, err := newRemoteServiceWithStore(root, false, true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	service.now = func() time.Time { return now }
	ctx := context.Background()
	result, err := service.RegisterSession(ctx, durableRegister(1, "expired", "client-sqlite", durableSecretA, "lease", "request"))
	if err != nil || result.Code != "" {
		t.Fatalf("register: %+v %v", result, err)
	}
	if err := service.StopSession(ctx, "expired"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(71 * time.Minute)
	if _, err := service.GetSession(ctx, "expired"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("did not expire: %v", err)
	}
	service.Close()
	service, err = NewSQLitePersistentRemoteService(root)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if _, err := service.GetSession(ctx, "expired"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expired registration restored: %v", err)
	}
}

func TestSQLiteRegistrationFailedMigrationCanRetryFromLegacy(t *testing.T) {
	root := t.TempDir()
	legacy, err := newRemoteService(root, false)
	if err != nil {
		t.Fatal(err)
	}
	result, err := legacy.RegisterSession(context.Background(), durableRegister(1, "retry-import", "client-sqlite", durableSecretA, "lease", "request"))
	if err != nil || result.Code != "" {
		t.Fatalf("register: %+v %v", result, err)
	}
	legacy.Close()
	engine, err := sqlitestore.Open(root, "registrations.db")
	if err != nil {
		t.Fatal(err)
	}
	engine.Close()
	database, err := sql.Open("sqlite", filepath.Join(root, "registrations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec("CREATE TRIGGER reject_import BEFORE INSERT ON records BEGIN SELECT RAISE(ABORT, 'blocked import'); END"); err != nil {
		t.Fatal(err)
	}
	if service, err := NewSQLitePersistentRemoteService(root); err == nil {
		service.Close()
		t.Fatal("failed import accepted")
	}
	if _, err := os.Stat(filepath.Join(root, result.Session.RegistrationID+".json")); err != nil {
		t.Fatalf("lost legacy backup: %v", err)
	}
	var rows, markers int
	if err := database.QueryRow("SELECT count(*) FROM records").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow("SELECT count(*) FROM metadata").Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || markers != 0 {
		t.Fatalf("failed migration committed data: rows=%d markers=%d", rows, markers)
	}
	if _, err := database.Exec("DROP TRIGGER reject_import"); err != nil {
		t.Fatal(err)
	}
	service, err := NewSQLitePersistentRemoteService(root)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	restored, err := service.GetSession(context.Background(), "retry-import")
	if err != nil || restored.RegistrationID != result.Session.RegistrationID {
		t.Fatalf("migration retry lost registration: %+v %v", restored, err)
	}
}
