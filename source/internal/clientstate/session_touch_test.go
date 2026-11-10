package clientstate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func oldSavedSession(t *testing.T, s *Store, n int, at time.Time) SavedConversation {
	t.Helper()
	saved := savedHandover(t, s, n)
	path := filepath.Join(s.sessionDir(saved.Key), "conn.json")
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return saved
}
func TestHandoverScansKeepUnrelatedSessionsOld(t *testing.T) {
	for _, scan := range []string{"candidates", "holder"} {
		t.Run(scan, func(t *testing.T) {
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			old := now.Add(-31 * 24 * time.Hour)
			var unrelated []SavedConversation
			for i := 1; i <= 3; i++ {
				unrelated = append(unrelated, oldSavedSession(t, s, i, old))
			}
			if scan == "candidates" {
				got, err := s.HandoverCandidates("unrelated-scope", "claude", "synthetic-client")
				if err != nil || len(got) != 0 {
					t.Fatal("unrelated candidates")
				}
			} else {
				selected := savedHandover(t, s, 10)
				selected.State.Scope = "selected-scope"
				selected.Key = SessionKey(selected.State.Scope, selected.State.Harness, selected.State.SessionID, selected.State.ClientID)
				if err := s.SaveSession(selected.Key, selected.State); err != nil {
					t.Fatal(err)
				}
				// Remove the helper's original same-registration record to avoid a revision tie.
				if err := os.RemoveAll(s.sessionDir(SessionKey("synthetic-scope", "claude", selected.State.SessionID, selected.State.ClientID))); err != nil {
					t.Fatal(err)
				}
				got, err := s.HandoverHolder(selected.Key, selected.State.Scope, "claude", selected.State.ClientID, false)
				if err != nil || got.Key != selected.Key {
					t.Fatal("holder fixture")
				}
			}
			stale := 0
			for _, saved := range unrelated {
				key := saved.Key
				info, err := os.Stat(filepath.Join(s.sessionDir(key), "conn.json"))
				if err != nil {
					t.Fatal(err)
				}
				if info.ModTime().Equal(old) {
					stale++
				}
			}
			result, err := s.Prune(now)
			if err != nil {
				t.Fatal(err)
			}
			if stale != 3 || result.Sessions != 3 {
				t.Fatalf("old records after scan=%d, pruned=%d; want3/3", stale, result.Sessions)
			}
		})
	}
}

func TestSessionTouchUseAndLiveLockProtectPrune(t *testing.T) {
	for _, kind := range []string{"use", "live-lock"} {
		t.Run(kind, func(t *testing.T) {
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			saved := oldSavedSession(t, s, 1, now.Add(-31*24*time.Hour))
			if kind == "use" {
				if _, found, err := s.LoadSession(saved.Key); err != nil || !found {
					t.Fatal("actual use")
				}
			} else {
				unlock, err := s.LockSession(saved.Key)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
				if _, err := s.HandoverCandidates("unrelated", "claude", "synthetic-client"); err != nil {
					t.Fatal(err)
				}
			}
			result, err := s.Prune(now)
			if err != nil || result.Sessions != 0 {
				t.Fatal("used/locked session pruned")
			}
			if _, err := os.Stat(filepath.Join(s.sessionDir(saved.Key), "conn.json")); err != nil {
				t.Fatal("session removed")
			}
		})
	}
}
