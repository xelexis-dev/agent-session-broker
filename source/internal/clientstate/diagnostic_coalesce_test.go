package clientstate

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func successfulDiagnostic(at time.Time) DiagnosticEvent {
	e := diagnosticFixture(at)
	e.Event, e.Reason, e.ErrorClass = "renew-success", "", ""
	return e
}

func TestDiagnosticSuccessWindowKeysAndFlush(t *testing.T) {
	s, _ := Open(t.TempDir())
	now := time.Now()
	key := RegistrationKey("scope", "codex", "11111111-1111-4111-8111-111111111111")
	other := RegistrationKey("other", "codex", "11111111-1111-4111-8111-111111111111")
	path := filepath.Join(s.Dir(), "diagnostics", "v2", key+".json")
	record := func(key string, seconds int) {
		t.Helper()
		at := now.Add(time.Duration(seconds) * time.Second)
		if err := s.recordDiagnosticAt(key, successfulDiagnostic(at.UTC()), at); err != nil {
			t.Fatal(err)
		}
	}
	record(key, 0)
	first, _ := os.Stat(path)
	record(key, 59)
	current, _ := os.Stat(path)
	if !os.SameFile(first, current) {
		t.Fatal("success persisted before 60 seconds")
	}
	record(other, 59)
	reopened, _ := Open(s.Dir())
	second, err := reopened.LoadDiagnostics(other)
	if err != nil || !second.Summary.LastRenewSuccess.Equal(now.Add(59*time.Second)) {
		t.Fatal("another session's first success was deferred", err)
	}
	record(key, 60)
	current, _ = os.Stat(path)
	if os.SameFile(first, current) {
		t.Fatal("success did not persist at 60 seconds")
	}
	record(key, 61)
	if err := s.FlushDiagnostics(); err != nil {
		t.Fatal(err)
	}
	disk, err := reopened.LoadDiagnostics(key)
	if err != nil || !disk.Summary.LastRenewSuccess.Equal(now.Add(61*time.Second)) {
		t.Fatal("flush lost latest success", err)
	}
	flushed, _ := os.Stat(path)
	if err := s.FlushDiagnostics(); err != nil {
		t.Fatal(err)
	}
	again, _ := os.Stat(path)
	if !os.SameFile(flushed, again) {
		t.Fatal("clean flush rewrote diagnostic")
	}
}

func TestDiagnosticImmediateEventsPreservePendingSuccess(t *testing.T) {
	for _, event := range []string{"renew-failed", "auth-transition", "reconnect-start", "reconnected", "receiver-exit", "activity-unavailable"} {
		t.Run(event, func(t *testing.T) {
			s, _ := Open(t.TempDir())
			key := RegistrationKey(event, "codex", "11111111-1111-4111-8111-111111111111")
			at := time.Now().UTC()
			s.RecordDiagnostic(key, successfulDiagnostic(at))
			latest := at.Add(time.Millisecond)
			s.RecordDiagnostic(key, successfulDiagnostic(latest))
			e := diagnosticFixture(at.Add(2 * time.Millisecond))
			e.Event, e.Reason, e.ErrorClass = event, "", ""
			if event == "auth-transition" {
				e.AuthState = "authorized"
			}
			if event == "activity-unavailable" {
				e.Reason = "unsupported"
			}
			if err := s.RecordDiagnostic(key, e); err != nil {
				t.Fatal(err)
			}
			reopened, _ := Open(s.Dir())
			disk, err := reopened.LoadDiagnostics(key)
			if err != nil || len(disk.Events) != 1 || disk.Events[0].Event != event || !disk.Summary.LastRenewSuccess.Equal(latest) {
				t.Fatal("immediate event/pending success missing", err)
			}
		})
	}
}

func TestDiagnosticPendingSuccessSurvivesIOFailure(t *testing.T) {
	s, _ := Open(t.TempDir())
	key := RegistrationKey("scope", "codex", "11111111-1111-4111-8111-111111111111")
	now := time.Now().UTC()
	s.RecordDiagnostic(key, successfulDiagnostic(now))
	latest := now.Add(time.Millisecond)
	s.RecordDiagnostic(key, successfulDiagnostic(latest))
	path := filepath.Join(s.Dir(), "diagnostics", "v2", key+".json")
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushDiagnostics(); err == nil {
		t.Fatal("flush hid IO failure")
	}
	out, err := s.LoadDiagnostics(key)
	if err == nil || !out.Summary.LastRenewSuccess.Equal(latest) {
		t.Fatal("IO failure lost in-memory freshness")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushDiagnostics(); err != nil {
		t.Fatal(err)
	}
	reopened, _ := Open(s.Dir())
	disk, err := reopened.LoadDiagnostics(key)
	if err != nil || !disk.Summary.LastRenewSuccess.Equal(latest) {
		t.Fatal("retry lost pending success", err)
	}
}

func TestDiagnosticConcurrentSuccessFreshness(t *testing.T) {
	s, _ := Open(t.TempDir())
	key := RegistrationKey("scope", "codex", "11111111-1111-4111-8111-111111111111")
	now := time.Now().UTC()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			if err := s.RecordDiagnostic(key, successfulDiagnostic(now.Add(time.Duration(i)*time.Millisecond))); err != nil {
				t.Error(err)
			}
			if _, err := s.LoadDiagnostics(key); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if err := s.FlushDiagnostics(); err != nil {
		t.Fatal(err)
	}
	reopened, _ := Open(s.Dir())
	disk, err := reopened.LoadDiagnostics(key)
	if err != nil || !disk.Summary.LastRenewSuccess.Equal(now.Add(19*time.Millisecond)) {
		t.Fatal("concurrent/out-of-order success lost", err)
	}
}

func TestDiagnosticSuccessCoalescesWrites(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := RegistrationKey("scope", "codex", "11111111-1111-4111-8111-111111111111")
	path := filepath.Join(s.Dir(), "diagnostics", "v2", key+".json")
	start := time.Now().UTC()
	var previous os.FileInfo
	replacements := 0
	for i := range 5 {
		if err := s.RecordDiagnostic(key, successfulDiagnostic(start.Add(time.Duration(i)*time.Millisecond))); err != nil {
			t.Fatal(err)
		}
		current, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if previous == nil || !os.SameFile(previous, current) {
			replacements++
		}
		previous = current
	}
	if replacements != 1 {
		t.Fatalf("5 successes within 60 seconds caused %d file replacements, want 1", replacements)
	}
	live, err := s.LoadDiagnostics(key)
	if err != nil || !live.Summary.LastRenewSuccess.Equal(start.Add(4*time.Millisecond)) {
		t.Fatal("memory freshness lost", err)
	}
	reopened, _ := Open(s.Dir())
	disk, err := reopened.LoadDiagnostics(key)
	if err != nil || !disk.Summary.LastRenewSuccess.Equal(start) {
		t.Fatal("successes were not deferred", err)
	}
	failure := diagnosticFixture(start.Add(time.Second))
	if err := s.RecordDiagnostic(key, failure); err != nil {
		t.Fatal(err)
	}
	current, _ := os.Stat(path)
	if os.SameFile(previous, current) {
		t.Fatal("failure did not persist immediately")
	}
	disk, err = reopened.LoadDiagnostics(key)
	if err != nil || !disk.Summary.LastFailureAt.Equal(failure.At) || !disk.Summary.LastRenewSuccess.Equal(live.Summary.LastRenewSuccess) {
		t.Fatal("failure did not flush pending success", err)
	}
}

func BenchmarkDiagnosticRenewSuccess(b *testing.B) {
	for _, events := range []int{0, DiagnosticEventLimit} {
		name := "empty"
		if events != 0 {
			name = "full"
		}
		b.Run(name, func(b *testing.B) {
			s, err := Open(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			key := RegistrationKey("scope", "codex", "11111111-1111-4111-8111-111111111111")
			for range events {
				if err := s.RecordDiagnostic(key, diagnosticFixture(time.Now().UTC())); err != nil {
					b.Fatal(err)
				}
			}
			if err := s.RecordDiagnostic(key, successfulDiagnostic(time.Now().UTC())); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := s.RecordDiagnostic(key, successfulDiagnostic(time.Now().UTC())); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
