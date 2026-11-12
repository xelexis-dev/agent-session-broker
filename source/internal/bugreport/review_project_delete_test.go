package bugreport

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewProjectDeletionKeepsUndecodableJournalRows(t *testing.T) {
	s, _, r := deletionFixture(t)
	if _, err := s.deletionDBLocked(); err != nil {
		t.Fatal(err)
	}
	other := []byte(`{"projectGenerationId":"other","v":2}`)
	future, _ := json.Marshal(map[string]any{"projectGenerationId": r.Reporter.ProjectGenerationID, "v": 2, "phase": "future"})
	if err := s.deletions.Batch(map[string][]byte{"bug-corrupt": []byte("{"), "bug-other": other, "bug-future": future}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete("admin", r.ID, r.ExpectedVersion); !errors.Is(err, ErrUnavailable) {
		t.Fatal("report deletion must remain strict", err)
	}
	warnings := captureDiagnostics(t)
	if err := s.DeleteProject("project", r.Reporter.ProjectGenerationID); err != nil {
		t.Fatal("corrupt metadata blocked project deletion", err)
	}
	if _, err := s.Get(r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("project report retained", err)
	}
	rows, err := s.deletions.List()
	if err != nil {
		t.Fatal(err)
	}
	if string(rows["bug-corrupt"]) != "{" || !bytes.Equal(rows["bug-other"], other) || rows["bug-future"] != nil {
		t.Fatal("lenient generation cleanup changed unrelated or malformed evidence")
	}
	if !strings.Contains(warnings.String(), "warning") {
		t.Fatal("missing journal warning")
	}
}
func TestReviewProjectDeletionContinuesWithUnavailableJournal(t *testing.T) {
	s, _, r := deletionFixture(t)
	if _, err := s.deletionDBLocked(); err != nil {
		t.Fatal(err)
	}
	if err := s.deletions.Close(); err != nil {
		t.Fatal(err)
	}
	s.deletions = nil
	if err := os.WriteFile(filepath.Join(s.directory, "deletions.db"), []byte("broken sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete("admin", r.ID, r.ExpectedVersion); !errors.Is(err, ErrUnavailable) {
		t.Fatal("report deletion unexpectedly accepted broken journal", err)
	}
	warnings := captureDiagnostics(t)
	if err := s.DeleteProject("project", r.Reporter.ProjectGenerationID); err != nil {
		t.Fatal("unavailable journal blocked project deletion", err)
	}
	if !strings.Contains(warnings.String(), "warning") {
		t.Fatal("missing unavailable journal warning")
	}
}
