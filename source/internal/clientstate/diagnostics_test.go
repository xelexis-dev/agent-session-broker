package clientstate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func diagnosticFixture(at time.Time) DiagnosticEvent {
	return DiagnosticEvent{At: at, SessionID: "11111111-1111-4111-8111-111111111111", RegistrationID: "AAAAAAAAAAAAAAAAAAAAAAAAAA", Generation: 7, Event: "renew-failed", Reason: "deadline", ErrorClass: "deadline"}
}
func TestDiagnosticsRetainRecentEventsAndPersistSummary(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := RegistrationKey("scope", "codex", "11111111-1111-4111-8111-111111111111")
	base := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 300; i++ {
		if err := s.RecordDiagnostic(key, diagnosticFixture(base.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	success := diagnosticFixture(base.Add(301 * time.Second))
	success.Event = "renew-success"
	success.Reason = ""
	success.ErrorClass = ""
	if err := s.RecordDiagnostic(key, success); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	out, err := reopened.LoadDiagnostics(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Events) != 256 || !out.Events[0].At.Equal(base.Add(44*time.Second)) || !out.Events[255].At.Equal(base.Add(299*time.Second)) {
		t.Fatal("ring did not retain only latest 256 events")
	}
	if !out.Summary.LastRenewSuccess.Equal(success.At) || !out.Summary.LastFailureAt.Equal(base.Add(299*time.Second)) || out.Summary.LastFailureClass != "deadline" {
		t.Fatal("summary lost success or last failure")
	}
	stat, err := os.Stat(filepath.Join(s.Dir(), "diagnostics", "v2", key+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if stat.Size() > 128*1024 {
		t.Fatal("diagnostic file exceeds 128 KiB")
	}
	if runtime.GOOS != "windows" && stat.Mode().Perm() != 0600 {
		t.Fatalf("diagnostic permissions %o", stat.Mode().Perm())
	}
	dir, err := os.Stat(filepath.Join(s.Dir(), "diagnostics"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && dir.Mode().Perm() != 0700 {
		t.Fatalf("diagnostic directory permissions %o", dir.Mode().Perm())
	}
}
func TestDiagnosticsRejectFreeTextAndUnsafeIdentifiers(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := RegistrationKey("scope", "codex", "11111111-1111-4111-8111-111111111111")
	for _, field := range []string{"session", "registration", "event", "reason", "class", "auth"} {
		t.Run(field, func(t *testing.T) {
			e := diagnosticFixture(time.Now())
			switch field {
			case "session":
				e.SessionID = "token=secret"
			case "registration":
				e.RegistrationID = "argv secret"
			case "event":
				e.Event = "message secret"
			case "reason":
				e.Reason = "https://secret.invalid/?token=secret"
			case "class":
				e.ErrorClass = "Authorization: Bearer secret"
			case "auth":
				e.AuthState = "secret"
			}
			if err := s.RecordDiagnostic(key, e); err == nil {
				t.Fatal("unsafe diagnostic accepted")
			}
		})
	}
	if err := s.RecordDiagnostic("../../escaped", diagnosticFixture(time.Now())); err == nil {
		t.Fatal("unsafe key accepted")
	}
	files, _ := filepath.Glob(filepath.Join(s.Dir(), "diagnostics", "*.json"))
	if len(files) != 0 {
		t.Fatal("unsafe event created a file")
	}
}
func TestDiagnosticsConcurrentWritesRemainSeparatedByBinding(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := RegistrationKey(fmt.Sprintf("scope-%d", i%2), "codex", "11111111-1111-4111-8111-111111111111")
			if err := s.RecordDiagnostic(key, diagnosticFixture(time.Now())); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < 2; i++ {
		key := RegistrationKey(fmt.Sprintf("scope-%d", i), "codex", "11111111-1111-4111-8111-111111111111")
		out, err := s.LoadDiagnostics(key)
		if err != nil || len(out.Events) != 20 {
			t.Fatalf("concurrent binding %d count=%d err=%v", i, len(out.Events), err)
		}
	}
}
func TestDiagnosticsRejectOversizeOrCorruptStoredData(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := RegistrationKey("scope", "codex", "11111111-1111-4111-8111-111111111111")
	if err := s.RecordDiagnostic(key, diagnosticFixture(time.Now())); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(s.Dir(), "diagnostics", "v2", key+".json")
	for _, raw := range []string{strings.Repeat(" ", 128*1024+1), `{"format":2,"summary":{"lastFailureClass":"token=secret"},"events":[]}`} {
		if err := os.WriteFile(filename, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.LoadDiagnostics(key); err == nil {
			t.Fatal("unsafe stored diagnostics accepted")
		}
	}
}

func TestDiagnosticsClaimEventsPreserveV1(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sid := "11111111-1111-4111-8111-111111111111"
	key := RegistrationKey("claim-test", "codex", sid)
	legacy := DiagnosticEvent{At: time.Now(), SessionID: sid, Event: "auth-transition", AuthState: "authorized"}
	path := filepath.Join(s.Dir(), "diagnostics", key+".json")
	raw, err := json.Marshal(Diagnostics{Events: []DiagnosticEvent{legacy}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"codex-claim-unconfirmed", "codex-claim-confirmed", "codex-claim-mismatch", "steer-recovery-acked", "steer-recovery-released", "steer-recovery-pending", "steer-recovery-skipped", "steer-recovery-ambiguous"} {
		e := DiagnosticEvent{At: time.Now(), SessionID: sid, Event: event}
		if v0172ValidDiagnostic(e) {
			t.Fatalf("frozen v1 accepted claim event %s", event)
		}
		if err := s.RecordDiagnostic(key, e); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("unsupported event changed v1 bytes")
	}
	merged, err := s.LoadDiagnostics(key)
	if err != nil {
		t.Fatal("merged diagnostic read failed")
	}
	seen := map[string]bool{}
	for _, e := range merged.Events {
		seen[e.Event] = true
	}
	for _, code := range []string{"auth-transition", "codex-claim-unconfirmed", "codex-claim-confirmed", "codex-claim-mismatch", "steer-recovery-acked", "steer-recovery-released", "steer-recovery-pending", "steer-recovery-skipped", "steer-recovery-ambiguous"} {
		if !seen[code] {
			t.Fatalf("merged load lost event %s", code)
		}
	}
	if err := s.RecordDiagnostic(key, legacy); err != nil {
		t.Fatal("legacy diagnostic write failed")
	}
}
