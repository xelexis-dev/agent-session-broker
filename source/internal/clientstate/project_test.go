package clientstate

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProjectKnowledgeMergesConcurrentSourcesAndScopes(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for _, source := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := s.SaveProjectKnowledge("a", map[string]ProjectKnowledge{source: {GenerationID: strings.Repeat("A", 26), Supported: true}}); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
	got, err := s.LoadProjectKnowledge("a")
	if err != nil || len(got) != 2 {
		t.Fatal("concurrent source lost")
	}
	other, err := s.LoadProjectKnowledge("b")
	if err != nil || len(other) != 0 {
		t.Fatal("scope leaked")
	}
}

func TestProjectTerminalDiagnosticIsPersisted(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	event := DiagnosticEvent{At: time.Now(), SessionID: "11111111-1111-4111-8111-111111111111", Event: "reconnect-failed", Reason: "project-deleted", ErrorClass: "project-deleted"}
	if err := s.RecordDiagnostic(RegistrationKey("scope", "codex", event.SessionID), event); err != nil {
		t.Fatal(err)
	}
	diagnostic, err := s.LoadDiagnostics(RegistrationKey("scope", "codex", event.SessionID))
	if err != nil || diagnostic.Summary.LastFailureCode != "project-deleted" || diagnostic.Summary.LastFailureClass != "project-deleted" {
		t.Fatal("project terminal diagnostic lost")
	}
}
