//go:build darwin

package broker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
)

func TestDarwinSQLiteRegistrationOwnsChildAndPreservesPinnedDestination(t *testing.T) {
	projectPath := t.TempDir()
	project, err := os.Open(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	defer project.Close()
	service, err := NewSQLitePersistentRemoteService(filepath.Join(pfs.DescriptorPath(project), "registrations"))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := project.Close(); err != nil {
		t.Fatal(err)
	}
	parked := filepath.Join(projectPath, "parked")
	if err := os.Rename(filepath.Join(projectPath, "registrations"), parked); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(projectPath, "registrations")); err != nil {
		t.Fatal(err)
	}
	result, err := service.RegisterSession(context.Background(), durableRegister(1, "pinned", "client-sqlite", durableSecretA, "lease", "request"))
	if err != nil || result.Code != "" {
		t.Fatalf("register: %+v %v", result, err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside write: %v %v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(parked, "registrations.db"))
	if err != nil || len(data) < 16 || string(data[:16]) != "SQLite format 3\x00" {
		t.Fatalf("database not in owned directory: %v", err)
	}
	service.Close()
	service, err = NewSQLitePersistentRemoteService(parked)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	restored, err := service.GetSession(context.Background(), "pinned")
	if err != nil || restored.RegistrationID != result.Session.RegistrationID {
		t.Fatalf("lost registration: %+v %v", restored, err)
	}
}
