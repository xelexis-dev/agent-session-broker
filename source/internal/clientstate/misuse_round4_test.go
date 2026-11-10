package clientstate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRound4DiagnosticsMalformedFilesRemainUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name     string
		raw      []byte
		readable bool
	}{
		{"oversized", []byte(`{"format":2,"extension":"` + strings.Repeat("x", DiagnosticByteLimit) + `"}`), false},
		{"too-deep", []byte(`{"format":2,"extension":` + strings.Repeat("[", 10001) + `0` + strings.Repeat("]", 10001) + `}`), false},
		{"invalid-utf8", append([]byte(`{"format":2,"extension":"`), 0xff, '"', '}'), false},
		{"empty", nil, false},
		{"future-999", []byte(`{"format":999,"summary":{},"events":[],"extension":"private-round4-sentinel"}`), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal("fixture open")
			}
			key := RegistrationKey("round4", "codex", "11111111-1111-4111-8111-111111111111")
			path := writeV2Fixture(t, s, key, tc.raw)
			out, err := s.LoadDiagnostics(key)
			if (err == nil) != tc.readable {
				t.Fatal("unexpected read classification")
			}
			visible, _ := json.Marshal(SafeDiagnostics(out))
			if bytes.Contains(visible, []byte("private-round4")) {
				t.Fatal("raw extension escaped")
			}
			if s.RecordDiagnostic(key, diagnosticFixture(time.Now().UTC())) == nil {
				t.Fatal("malformed/read-only file accepted a write")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(tc.raw, after) {
				t.Fatal("file changed")
			}
		})
	}
}

func TestRound4DiagnosticsDuplicateAndNestedFields(t *testing.T) {
	s, _ := Open(t.TempDir())
	key := RegistrationKey("round4", "codex", "11111111-1111-4111-8111-111111111111")
	nested := strings.Repeat("[", 64) + `"private-nested-sentinel"` + strings.Repeat("]", 64)
	raw := []byte(`{"format":999,"format":2,"summary":{},"events":[],"extension":` + nested + `}`)
	path := writeV2Fixture(t, s, key, raw)
	d, err := s.LoadDiagnostics(key)
	if err != nil || d.Format != 2 {
		t.Fatal("JSON last duplicate value rule changed")
	}
	if err = s.RecordDiagnostic(key, diagnosticFixture(time.Now().UTC())); err != nil {
		t.Fatal("bounded nested extension rejected")
	}
	after, _ := os.ReadFile(path)
	var fields map[string]json.RawMessage
	if json.Unmarshal(after, &fields) != nil || string(fields["extension"]) != nested {
		t.Fatal("nested extension lost")
	}
	d, err = s.LoadDiagnostics(key)
	safe, _ := json.Marshal(SafeDiagnostics(d))
	if err != nil || bytes.Contains(safe, []byte("private-nested")) {
		t.Fatal("extension escaped")
	}
}

func TestRound4DiagnosticsRejectSymlinksWithoutChangingTargets(t *testing.T) {
	for _, where := range []string{"file", "v2", "diagnostics", "lock"} {
		t.Run(where, func(t *testing.T) {
			s, _ := Open(t.TempDir())
			key := RegistrationKey("round4", "codex", "11111111-1111-4111-8111-111111111111")
			outside := t.TempDir()
			raw := []byte(`{"format":2,"summary":{},"events":[],"extension":"private-link-sentinel"}`)
			external := filepath.Join(outside, "v2", key+".json")
			os.MkdirAll(filepath.Dir(external), 0755)
			os.Chmod(filepath.Dir(external), 0755)
			os.WriteFile(external, raw, 0600)
			parent := filepath.Join(s.Dir(), "diagnostics")
			os.MkdirAll(parent, 0700)
			var link, target string
			switch where {
			case "diagnostics":
				os.Remove(parent)
				link, target = parent, outside
			case "v2":
				link, target = filepath.Join(parent, "v2"), filepath.Join(outside, "v2")
			case "file":
				os.MkdirAll(filepath.Join(parent, "v2"), 0700)
				link, target = filepath.Join(parent, "v2", key+".json"), external
			case "lock":
				os.MkdirAll(filepath.Join(parent, "v2"), 0700)
				link, target = filepath.Join(parent, "v2", key+".lock"), external
			}
			if os.Symlink(target, link) != nil {
				t.Skip("symlink unavailable")
			}
			if where != "lock" {
				if _, err := s.LoadDiagnostics(key); err == nil {
					t.Error("symlink-backed diagnostics were read")
				}
			}
			if s.RecordDiagnostic(key, diagnosticFixture(time.Now().UTC())) == nil {
				t.Error("symlink-backed diagnostics accepted a write")
			}
			after, _ := os.ReadFile(external)
			info, _ := os.Stat(filepath.Dir(external))
			if !bytes.Equal(raw, after) || info.Mode().Perm() != 0755 {
				t.Error("external fixture changed")
			}
			info, err := os.Lstat(link)
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Error("symlink was replaced")
			}
		})
	}
}
