//go:build darwin

package broker

import (
	"context"
	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDarwinRegistrationStoreOwnsChildDescriptor(t *testing.T) {
	projectPath := t.TempDir()
	project, err := os.Open(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	defer project.Close()
	service, err := newRemoteService(filepath.Join(pfs.DescriptorPath(project), "registrations"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	// Closing the caller's parent pin must not invalidate the owned child pin.
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
	result, err := service.RegisterSession(context.Background(), durableRegister(1, "owned-child", "client-descriptor", durableSecretA, "lease-child", "request-child"))
	if err != nil || result.Code != "" {
		t.Fatalf("register: %+v %v", result, err)
	}
	records, err := service.store.loadAll(time.Now())
	if err != nil || len(records) != 1 {
		t.Fatalf("load: %v %v", records, err)
	}
	if _, err := os.Stat(filepath.Join(parked, result.Session.RegistrationID+".json")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside write: %v %v", entries, err)
	}
	service.store.quarantine(result.Session.RegistrationID+".json", time.Now())
	records, err = service.store.loadAll(time.Now())
	if err != nil || len(records) != 0 {
		t.Fatalf("quarantine: %v %v", records, err)
	}
	if err := service.store.save(&recordsForPinTest); err != nil {
		t.Fatal(err)
	}
	if err := service.store.remove(recordsForPinTest.RegistrationID); err != nil {
		t.Fatal(err)
	}
	operationPath := service.store.root
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := pfs.Stat(operationPath); err == nil {
		t.Fatal("owned descriptor leaked after Close")
	}
	if err := service.store.save(&recordsForPinTest); err == nil {
		t.Fatal("closed store accepted write")
	}
}

var recordsForPinTest = registrationRecord{RegistrationID: "AAAAAAAAAAAAAAAAAAAAAAAAAA"}
