package clientstate

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
	"time"
)

func TestSessionKeySeparatesClientInstances(t *testing.T) {
	a := SessionKey("https://broker/mcp?project=brk", "claude", "11111111-1111-4111-8111-111111111111", "pc-one")
	b := SessionKey("https://broker/mcp?project=brk", "claude", "11111111-1111-4111-8111-111111111111", "pc-two")
	if a == b {
		t.Fatal("client instance is not part of the session key")
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(a) {
		t.Fatalf("session key is not a usable file name: %q", a)
	}
	if a != SessionKey("https://broker/mcp?project=brk", "claude", "11111111-1111-4111-8111-111111111111", "pc-one") {
		t.Fatal("session key is not stable")
	}
}

func TestSessionStateRoundTripsUnderItsOwnLock(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := SessionKey("https://broker/mcp?project=brk", "codex", "22222222-2222-4222-8222-222222222222", "pc-one")
	if _, found, err := s.LoadSession(key); err != nil || found {
		t.Fatalf("empty store: found=%v err=%v", found, err)
	}
	expires := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	state := SessionState{
		SchemaVersion: 1, Scope: "https://broker/mcp?project=brk", Project: "brk", Harness: "codex",
		SessionID: "22222222-2222-4222-8222-222222222222", ClientID: "pc-one",
		Registration: Registration{Secret: "resume-secret", RegistrationID: "REG1", Name: "agent-a", Revision: 3},
		Credential:   &SessionCredential{AccessToken: "asb_s_value", TokenType: "Bearer", ExpiresAt: expires, SessionGrantID: "grant-1", Nickname: "agent-a", Scopes: []string{"session:register"}, AdmissionMode: "auto"},
		Direct:       &DirectAuthorization{RequestID: "direct-request", RequestedNickname: "agent-a", CreatedAt: expires, Status: "redeemed", Restricted: true},
		Device:       &DeviceAuthorization{RequestID: "req-1", Status: DeviceRedeemed},
	}
	if err := s.SaveSession(key, state); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sessions", key, "conn.json")
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("session state file: %v", err)
	}
	loaded, found, err := s.LoadSession(key)
	if err != nil || !found {
		t.Fatalf("load: found=%v err=%v", found, err)
	}
	if loaded.Registration != state.Registration || loaded.Credential == nil || !reflect.DeepEqual(loaded.Credential, state.Credential) || loaded.Device == nil || *loaded.Device != *state.Device || loaded.ClientID != "pc-one" || !reflect.DeepEqual(loaded.Direct, state.Direct) {
		t.Fatal("round trip lost fields")
	}
	release, err := s.LockSession(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LockSession(key); !errors.Is(err, ErrLocked) {
		t.Fatalf("second holder was admitted: %v", err)
	}
	release()
	release, err = s.LockSession(key)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	release()
}

func TestLoadSessionRejectsCorruptState(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := SessionKey("https://broker/mcp?project=brk", "claude", "33333333-3333-4333-8333-333333333333", "pc-one")
	if err := os.MkdirAll(filepath.Join(dir, "sessions", key), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"{not json", `{"schema_version":1,"registration":{"secret":""}}`, `{"schema_version":2,"registration":{"secret":"x"}}`} {
		if err := os.WriteFile(filepath.Join(dir, "sessions", key, "conn.json"), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.LoadSession(key); err == nil {
			t.Fatalf("corrupt state accepted: %s", raw)
		}
	}
}

func TestAdoptLegacyRegistrationCopiesOwnershipAndKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	scope, harness, sessionID := "https://broker/mcp?project=brk", "claude", "44444444-4444-4444-8444-444444444444"
	legacyKey := RegistrationKey(scope, harness, sessionID)
	legacy := Registration{RegistrationID: "REGLEGACY", Name: "owner", Secret: "legacy-secret", Revision: 7, Project: "brk", Harness: harness, SessionID: sessionID, PendingRequestID: "req-9", PendingLease: "lease-9", PendingRevision: 7}
	if err := s.SaveRegistration(legacyKey, legacy); err != nil {
		t.Fatal(err)
	}
	key := SessionKey(scope, harness, sessionID, "pc-one")
	adopted, err := s.AdoptLegacyRegistration(key, legacyKey)
	if err != nil || !adopted {
		t.Fatalf("adopt: %v %v", adopted, err)
	}
	state, found, err := s.LoadSession(key)
	if err != nil || !found {
		t.Fatalf("adopted state missing: %v", err)
	}
	if state.Registration.Secret != "legacy-secret" || state.Registration.RegistrationID != "REGLEGACY" || state.Registration.Revision != 7 || state.Registration.PendingRequestID != "req-9" || state.Registration.PendingLease != "lease-9" || state.Registration.PendingRevision != 7 {
		t.Fatalf("ownership fields not carried over: %+v", state.Registration)
	}
	if _, found, err := s.LoadRegistration(legacyKey); err != nil || !found {
		t.Fatal("legacy file was removed or damaged")
	}
	// An existing session state is never overwritten by a later adoption.
	state.Credential = &SessionCredential{AccessToken: "asb_s_new", TokenType: "Bearer"}
	if err := s.SaveSession(key, state); err != nil {
		t.Fatal(err)
	}
	if adopted, err := s.AdoptLegacyRegistration(key, legacyKey); err != nil || adopted {
		t.Fatalf("second adoption touched existing state: %v %v", adopted, err)
	}
	if again, _, _ := s.LoadSession(key); again.Credential == nil {
		t.Fatal("adoption overwrote the session credential")
	}
}

func TestAdoptLegacyRegistrationRefusesLiveHolderAndMissingFile(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	scope, harness, sessionID := "https://broker/mcp?project=brk", "codex", "55555555-5555-4555-8555-555555555555"
	legacyKey := RegistrationKey(scope, harness, sessionID)
	key := SessionKey(scope, harness, sessionID, "pc-one")
	if adopted, err := s.AdoptLegacyRegistration(key, legacyKey); err != nil || adopted {
		t.Fatalf("missing legacy file reported adoption: %v %v", adopted, err)
	}
	if err := s.SaveRegistration(legacyKey, Registration{Secret: "legacy-secret", RegistrationID: "REG", Project: "brk", Harness: harness, SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	release, err := s.Lock(legacyKey)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := s.AdoptLegacyRegistration(key, legacyKey); !errors.Is(err, ErrLocked) {
		t.Fatalf("adoption ignored a live legacy client: %v", err)
	}
	if _, found, _ := s.LoadSession(key); found {
		t.Fatal("adoption wrote state while the legacy client was live")
	}
}
