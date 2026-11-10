package clientstate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestStoreCreatesStableClientIDAndRegistrationFiles(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.ClientID()
	if err != nil || !clientIDPattern.MatchString(first) {
		t.Fatalf("client id: %q %v", first, err)
	}
	second, _ := store.ClientID()
	if second != first {
		t.Fatalf("client id changed: %q != %q", first, second)
	}
	key := RegistrationKey("brk", "claude", "11111111-1111-4111-8111-111111111111")
	if _, found, err := store.LoadRegistration(key); found || err != nil {
		t.Fatalf("missing registration reported as present: %v %v", found, err)
	}
	secret := NewSecret()
	if len(secret) != 64 {
		t.Fatalf("secret length %d", len(secret))
	}
	saved := Registration{RegistrationID: "ABCDEFGHIJKLMNOPQRSTUVWXYZ", Name: "pc-a", Secret: secret, Revision: 3, Project: "brk", Harness: "claude", SessionID: "11111111-1111-4111-8111-111111111111"}
	if err := store.SaveRegistration(key, saved); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := store.LoadRegistration(key)
	if err != nil || !found || loaded != saved {
		t.Fatalf("round trip: %+v %v %v", loaded, found, err)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "registrations", "*.tmp")); len(files) != 0 {
		t.Fatalf("temporary files left: %v", files)
	}
	pending := Registration{Secret: secret, Project: "brk", Harness: "claude", SessionID: saved.SessionID, PendingRequestID: "req", PendingLease: "lease"}
	if err := store.SaveRegistration(key, pending); err != nil {
		t.Fatal(err)
	}
	if loaded, found, err := store.LoadRegistration(key); err != nil || !found || loaded != pending {
		t.Fatalf("pending-only state rejected: %+v %v %v", loaded, found, err)
	}
	if err := os.WriteFile(store.registrationPath(key), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LoadRegistration(key); err == nil {
		t.Fatal("corrupt registration state accepted")
	}
	if err := store.RemoveRegistration(key); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.LoadRegistration(key); found {
		t.Fatal("removed registration still present")
	}
}

func TestClientIDIsCreatedOnceUnderConcurrentFirstUse(t *testing.T) {
	dir := t.TempDir()
	results := make(chan string, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store, err := Open(dir)
			if err != nil {
				results <- "error:" + err.Error()
				return
			}
			id, err := store.ClientID()
			if err != nil {
				results <- "error:" + err.Error()
				return
			}
			results <- id
		}()
	}
	wg.Wait()
	close(results)
	seen := map[string]bool{}
	for id := range results {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("concurrent first use produced %d identities: %v", len(seen), seen)
	}
	for id := range seen {
		if strings.HasPrefix(id, "error:") {
			t.Fatal(id)
		}
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "client-id.tmp*")); len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
}

func TestStoreLockIsExclusivePerRegistration(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	release, err := store.Lock("one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Lock("one"); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock holder admitted: %v", err)
	}
	other, err := store.Lock("two")
	if err != nil {
		t.Fatalf("independent registration blocked: %v", err)
	}
	other()
	release()
	again, err := store.Lock("one")
	if err != nil {
		t.Fatalf("released lock not reacquirable: %v", err)
	}
	again()
}

func TestDefaultDirHonoursOverride(t *testing.T) {
	override := t.TempDir()
	t.Setenv(EnvDir, override)
	dir, err := DefaultDir()
	if err != nil || dir != override {
		t.Fatalf("override ignored: %q %v", dir, err)
	}
	t.Setenv(EnvDir, "relative/path")
	if _, err := DefaultDir(); err == nil {
		t.Fatal("relative override accepted")
	}
	t.Setenv(EnvDir, "")
	dir, err = DefaultDir()
	if err != nil || !strings.HasSuffix(dir, "agent-session-broker") {
		t.Fatalf("default dir: %q %v", dir, err)
	}
}
