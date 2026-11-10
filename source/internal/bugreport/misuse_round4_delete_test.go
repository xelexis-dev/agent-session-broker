package bugreport

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
)

type misuseDeletionFailure struct {
	reportDatabase
	failure error
}

func (d misuseDeletionFailure) Batch(map[string][]byte, []string) error { return d.failure }

func TestMisuseK2DeletionJournalFaults(t *testing.T) {
	for _, mode := range []string{"corrupt-file", "corrupt-row", "permission-dir", "permission-file", "permission", "disk-full"} {
		t.Run(mode, func(t *testing.T) {
			s, _, r := deletionFixture(t)
			if runtime.GOOS == "windows" && mode == "permission-dir" {
				t.Skip("POSIX directory chmod repair is not a Windows DACL fixture")
			}
			switch mode {
			case "permission-file":
				if os.Getuid() == 0 {
					t.Skip("permission semantics require unprivileged owner")
				}
				if _, err := s.deletionDBLocked(); err != nil {
					t.Fatal(err)
				}
				if err := s.deletions.Close(); err != nil {
					t.Fatal(err)
				}
				s.deletions = nil
				path := filepath.Join(s.directory, "deletions.db")
				if err := os.Chmod(path, 0000); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(path, 0600)
			case "permission-dir":
				if os.Getuid() == 0 {
					t.Skip("permission semantics require unprivileged owner")
				}
				if err := os.Chmod(s.directory, 0500); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(s.directory, 0700)
			case "corrupt-file":
				if err := os.WriteFile(filepath.Join(s.directory, "deletions.db"), []byte("broken sqlite"), 0600); err != nil {
					t.Fatal(err)
				}
			default:
				if _, err := s.deletionDBLocked(); err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "corrupt-row":
					if err := s.deletions.Put("bug-corrupt", []byte("{")); err != nil {
						t.Fatal(err)
					}
				case "permission":
					s.deletions = misuseDeletionFailure{s.deletions, os.ErrPermission}
				case "disk-full":
					s.deletions = misuseDeletionFailure{s.deletions, syscall.ENOSPC}
				}
			}
			if mode == "permission-dir" {
				// The shared SQLite opener restores an owned directory to 0700.
				if d, err := s.Delete("admin", r.ID, r.ExpectedVersion); err != nil || d.Phase != "deleted" {
					t.Fatal("owned directory repair", err)
				}
				info, err := os.Stat(s.directory)
				if err != nil || info.Mode().Perm() != 0700 {
					t.Fatal("directory mode not restored")
				}
				return
			}
			if _, err := s.Delete("admin", r.ID, r.ExpectedVersion); !errors.Is(err, ErrUnavailable) {
				t.Fatal("journal fault was accepted", err)
			}
			if _, err := s.Get(r.ID); err != nil {
				t.Fatal("journal fault lost content", err)
			}
		})
	}
}

func TestMisuseK2DeleteStatusProjectRemovalRace(t *testing.T) {
	for i := 0; i < 12; i++ {
		s, _, r := deletionFixture(t)
		start := make(chan struct{})
		var wg sync.WaitGroup
		results := make(chan error, 3)
		for op := 0; op < 3; op++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				var err error
				switch op {
				case 0:
					_, err = s.Delete("admin", r.ID, r.ExpectedVersion)
				case 1:
					_, _, err = s.SetResolution(r.ID, "resolved", "", Actor{UserID: "admin", Source: "panel"}, r.ExpectedVersion)
				case 2:
					err = s.DeleteProject("project", r.Reporter.ProjectGenerationID)
				}
				results <- err
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		for err := range results {
			if err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrNotFound) {
				t.Fatal("unexpected race result", err)
			}
		}
		if _, err := s.Get(r.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("project removal retained report", err)
		}
		if _, err := s.DeletionFor("admin", r.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("project removal retained tombstone", err)
		}
	}
}
