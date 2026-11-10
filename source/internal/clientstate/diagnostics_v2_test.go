package clientstate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDiagnosticsV1FutureEventReadOnly(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := RegistrationKey("scope", "codex", "11111111-1111-4111-8111-111111111111")
	e := diagnosticFixture(time.Now().UTC())
	e.Event = "codex-claim-mismatch"
	raw, _ := json.Marshal(Diagnostics{Events: []DiagnosticEvent{e}})
	path := filepath.Join(s.Dir(), "diagnostics", key+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := s.LoadDiagnostics(key)
	if err != nil || len(out.Events) != 1 || out.Events[0].Event != e.Event {
		t.Error("future v1 event must remain readable", err)
	}
	if err := s.RecordDiagnostic(key, diagnosticFixture(e.At.Add(time.Second))); err != nil {
		t.Error("future v1 event must not block a new v2 record", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("record changed frozen v1 bytes")
	}
}

func writeV2Fixture(t *testing.T, s *Store, key string, raw []byte) string {
	t.Helper()
	path := filepath.Join(s.Dir(), "diagnostics", "v2", key+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiagnosticsV2PreservesUnknownFieldsAndCodes(t *testing.T) {
	s, _ := Open(t.TempDir())
	key := RegistrationKey("future", "codex", "11111111-1111-4111-8111-111111111111")
	e := diagnosticFixture(time.Now().UTC())
	e.Event = "future-code"
	raw, _ := json.Marshal(Diagnostics{Events: []DiagnosticEvent{e}})
	var obj map[string]any
	json.Unmarshal(raw, &obj)
	obj["format"] = 2
	obj["future"] = map[string]any{"nested": []any{"private-sentinel-one", float64(7)}}
	obj["summary"].(map[string]any)["extension"] = map[string]any{"hidden": "private-sentinel-two"}
	obj["summary"].(map[string]any)["lastFailureClass"] = "private-secret-like-code"
	obj["events"].([]any)[0].(map[string]any)["extension"] = map[string]any{"hidden": "private-sentinel-three"}
	raw, _ = json.Marshal(obj)
	path := writeV2Fixture(t, s, key, raw)
	if err := s.RecordDiagnostic(key, diagnosticFixture(e.At.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	var next map[string]any
	json.Unmarshal(after, &next)
	if !reflect.DeepEqual(obj["future"], next["future"]) || !reflect.DeepEqual(obj["summary"].(map[string]any)["extension"], next["summary"].(map[string]any)["extension"]) || !reflect.DeepEqual(obj["events"].([]any)[0].(map[string]any)["extension"], next["events"].([]any)[0].(map[string]any)["extension"]) {
		t.Fatal("extension JSON did not survive rewrite")
	}
	out, err := s.LoadDiagnostics(key)
	if err != nil || out.Events[0].Event != "future-code" {
		t.Fatal("unknown event lost", err)
	}
	view := SafeDiagnostics(out)
	visible, _ := json.Marshal(view)
	if bytes.Contains(visible, []byte("private-")) || bytes.Contains(visible, []byte("future-code")) || view.Summary.UnknownCodes != 1 || view.Events[0].Event != "unknown" {
		t.Fatal("unsafe projection or unknown count")
	}
	e.Event = "future-code"
	if s.RecordDiagnostic(key, e) == nil {
		t.Fatal("write API accepted unknown event")
	}
}

func TestDiagnosticsFutureFormatAndCorruptionNeverOverwrite(t *testing.T) {
	for _, raw := range []string{`{"format":3,"summary":{},"events":[],"future":{"private":1}}`, `{"format":2,`} {
		t.Run(raw[:11], func(t *testing.T) {
			s, _ := Open(t.TempDir())
			key := RegistrationKey("future", "codex", "11111111-1111-4111-8111-111111111111")
			path := writeV2Fixture(t, s, key, []byte(raw))
			out, err := s.LoadDiagnostics(key)
			if strings.Contains(raw, `"format":3`) && (err != nil || out.Format != 3) {
				t.Fatal("future format not readable", err)
			}
			if s.RecordDiagnostic(key, diagnosticFixture(time.Now().UTC())) == nil {
				t.Fatal("read-only/corrupt file overwritten")
			}
			after, _ := os.ReadFile(path)
			if string(after) != raw {
				t.Fatal("protected bytes changed")
			}
		})
	}
}

func TestDiagnosticsDropsOnlyBadGrammarEvent(t *testing.T) {
	s, _ := Open(t.TempDir())
	key := RegistrationKey("grammar", "codex", "11111111-1111-4111-8111-111111111111")
	e := diagnosticFixture(time.Now().UTC())
	events := []DiagnosticEvent{e, e, e, e}
	events[0].Event = "Bad"
	events[1].Reason = "contains space"
	events[2].ErrorClass = strings.Repeat("a", 49)
	events[3].Event = "future-good-code"
	raw, _ := json.Marshal(Diagnostics{Format: 2, Events: events})
	writeV2Fixture(t, s, key, raw)
	out, err := s.LoadDiagnostics(key)
	if err != nil || len(out.Events) != 1 || out.Events[0].Event != "future-good-code" {
		t.Fatal("bad event poisoned file", err)
	}
	if err := s.RecordDiagnostic(key, e); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnosticsMergeWithoutDedupeAndIgnoreCorruptLegacy(t *testing.T) {
	s, _ := Open(t.TempDir())
	key := RegistrationKey("merge", "codex", "11111111-1111-4111-8111-111111111111")
	e := diagnosticFixture(time.Now().UTC())
	raw, _ := json.Marshal(Diagnostics{Events: []DiagnosticEvent{e}})
	legacy := filepath.Join(s.Dir(), "diagnostics", key+".json")
	os.MkdirAll(filepath.Dir(legacy), 0700)
	os.WriteFile(legacy, raw, 0600)
	if err := s.RecordDiagnostic(key, e); err != nil {
		t.Fatal(err)
	}
	out, err := s.LoadDiagnostics(key)
	if err != nil || len(out.Events) != 2 {
		t.Fatal("merge deduplicated or lost source", err)
	}
	os.WriteFile(legacy, []byte(`{`), 0600)
	if err := s.RecordDiagnostic(key, e); err != nil {
		t.Fatal("corrupt v1 blocked v2", err)
	}
	out, err = s.LoadDiagnostics(key)
	if err != nil || len(out.Events) != 2 {
		t.Fatal("corrupt v1 must leave v2 usable", err)
	}
}

func TestDiagnosticsV2ByteLimitTruncatesOldestExtensions(t *testing.T) {
	s, _ := Open(t.TempDir())
	key := RegistrationKey("bytes", "codex", "11111111-1111-4111-8111-111111111111")
	now := time.Now().UTC()
	var events []DiagnosticEvent
	for i := 0; i < 25; i++ {
		e := diagnosticFixture(now.Add(time.Duration(i) * time.Second))
		e.extra = map[string]json.RawMessage{"extension": json.RawMessage(`"` + strings.Repeat("x", 4096) + `"`)}
		events = append(events, e)
	}
	raw, _ := json.Marshal(Diagnostics{Format: 2, Events: events})
	if len(raw) >= DiagnosticByteLimit {
		t.Fatal("invalid test fixture")
	}
	path := writeV2Fixture(t, s, key, raw)
	for i := 25; i < 200; i++ {
		if err := s.RecordDiagnostic(key, diagnosticFixture(now.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := os.ReadFile(path)
	out, err := s.LoadDiagnostics(key)
	if err != nil || len(after) > DiagnosticByteLimit || len(out.Events) >= 200 || out.Events[len(out.Events)-1].At != now.Add(199*time.Second) {
		t.Fatal("byte truncation failed", err)
	}
}

func TestDiagnosticsPruneV2AndOldPruneSelection(t *testing.T) {
	s, _ := Open(t.TempDir())
	key := RegistrationKey("prune", "codex", "11111111-1111-4111-8111-111111111111")
	now := time.Now()
	if err := s.RecordDiagnostic(key, diagnosticFixture(now.UTC())); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir(), "diagnostics", "v2", key+".json")
	old := now.Add(-15 * 24 * time.Hour)
	os.Chtimes(path, old, old)
	// Tag 0.17.2 iterates direct children and filters by .json before key validation.
	entries, _ := os.ReadDir(filepath.Join(s.Dir(), "diagnostics"))
	for _, entry := range entries {
		if entry.Name() == "v2" && strings.HasSuffix(entry.Name(), ".json") {
			t.Fatal("old prune would select v2")
		}
	}
	unlock, err := lockFile(filepath.Join(s.Dir(), "diagnostics", "v2", key+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Prune(now)
	if err != nil || r.Diagnostics != 0 {
		t.Fatal("prune ignored v2 writer lock", err)
	}
	unlock()
	r, err = s.Prune(now.Add(25 * time.Hour))
	if err != nil || r.Diagnostics != 1 {
		t.Fatal("v2 idle file not pruned", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("v2 file remains")
	}
}

func TestDiagnosticsV1StillReadableBy0172Rules(t *testing.T) {
	s, _ := Open(t.TempDir())
	key := RegistrationKey("rollback", "codex", "11111111-1111-4111-8111-111111111111")
	e := diagnosticFixture(time.Now().UTC())
	raw, _ := json.Marshal(Diagnostics{Events: []DiagnosticEvent{e}})
	legacy := filepath.Join(s.Dir(), "diagnostics", key+".json")
	os.MkdirAll(filepath.Dir(legacy), 0700)
	os.WriteFile(legacy, raw, 0600)
	for _, code := range []string{"codex-claim-unconfirmed", "codex-claim-confirmed", "codex-claim-mismatch", "steer-uncertain-retired", "steer-delivered", "steer-rejected", "steer-unverified", "steer-disabled-off", "steer-disabled-ack-required", "steer-disabled-unconfirmed-thread", "steer-disabled-unsupported-codex", "steer-disabled-unverified-host"} {
		e.Event = code
		if err := s.RecordDiagnostic(key, e); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := os.ReadFile(legacy)
	if !bytes.Equal(raw, after) {
		t.Fatal("rollback input changed")
	}
	// Use plain types so the new tolerant decoder cannot hide a rejected old event.
	type oldEvent DiagnosticEvent
	type oldSummary DiagnosticSummary
	var old struct {
		Summary oldSummary `json:"summary"`
		Events  []oldEvent `json:"events"`
	}
	if err := json.Unmarshal(after, &old); err != nil || len(old.Events) != 1 || !v0172ValidSummary(DiagnosticSummary(old.Summary)) || !v0172ValidDiagnostic(DiagnosticEvent(old.Events[0])) {
		t.Fatal("frozen 0.17.2 validators reject v1", err)
	}
}

func TestDiagnosticsPruneKeepsV2LockAndRejectsSymlinkParent(t *testing.T) {
	t.Run("isolated v2 lock", func(t *testing.T) {
		s, _ := Open(t.TempDir())
		key := RegistrationKey("prune", "codex", "11111111-1111-4111-8111-111111111111")
		now := time.Now()
		s.RecordDiagnostic(key, diagnosticFixture(now.UTC()))
		legacy := filepath.Join(s.Dir(), "diagnostics", key+".json")
		os.WriteFile(legacy, []byte(`{}`), 0600)
		old := now.Add(-15 * 24 * time.Hour)
		os.Chtimes(legacy, old, old)
		lock := filepath.Join(s.Dir(), "diagnostics", "v2", key+".lock")
		before, _ := os.Stat(lock)
		result, err := s.Prune(now)
		after, _ := os.Stat(lock)
		if err != nil || result.Diagnostics != 1 || !os.SameFile(before, after) {
			t.Fatal("prune changed the isolated v2 lock", err)
		}
	})
	t.Run("symlink parent", func(t *testing.T) {
		s, _ := Open(t.TempDir())
		outside := t.TempDir()
		key := RegistrationKey("escape", "codex", "11111111-1111-4111-8111-111111111111")
		now := time.Now()
		target := filepath.Join(outside, "v2", key+".json")
		os.MkdirAll(filepath.Dir(target), 0700)
		os.WriteFile(target, []byte(`{}`), 0600)
		old := now.Add(-15 * 24 * time.Hour)
		os.Chtimes(target, old, old)
		if err := os.Symlink(outside, filepath.Join(s.Dir(), "diagnostics")); err != nil {
			t.Skip("symlink fixture unavailable")
		}
		if _, err := s.Prune(now); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatal("prune crossed a symlink parent")
		}
	})
}
