package broker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

type batchRemovalAdmission struct {
	ownerTestAdmission
	deny string
}

func (a *batchRemovalAdmission) CheckSession(_ context.Context, s Session) error {
	if s.Name == a.deny {
		return ErrSessionIdentityUnauthorized
	}
	return nil
}

func TestRemoveDetachedBatchFailuresAndIndividualChecks(t *testing.T) {
	for _, kind := range []string{"policy", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "sqlite" && runtime.GOOS != "windows" && os.Getuid() == 0 {
				t.Skip("requires unprivileged directory permissions")
			}
			root := t.TempDir()
			calls := 0
			fail := kind == "policy"
			s, err := NewSQLitePersistentRemoteServiceWithBatchCleanup(root, func(proofs []access.DetachedRemovalProof) error {
				calls++
				if len(proofs) != 2 {
					t.Error("batch included live, duplicate or unauthorized proof")
				}
				if fail {
					return errors.New("synthetic policy failure")
				}
				return nil
			})
			if err != nil {
				t.Fatal("open")
			}
			defer s.Close()
			auth := &batchRemovalAdmission{}
			s.SetAdmission(auth)
			var selectors []string
			for i := 1; i <= 4; i++ {
				req := durableRegister(i, fmt.Sprintf("batch-%d", i), fmt.Sprintf("batch-client-%d", i), durableSecretA, fmt.Sprintf("lease-%d", i), fmt.Sprintf("request-%d", i))
				out, err := s.RegisterSession(t.Context(), req)
				if err != nil || out.Code != "" {
					t.Fatal("register")
				}
				selector := SelectorRegistration + out.Session.RegistrationID
				selectors = append(selectors, selector)
				if i != 3 {
					if err := s.StopSession(t.Context(), selector); err != nil {
						t.Fatal("detach")
					}
				}
			}
			auth.deny = "batch-4"
			selectors = append(selectors, selectors[0], "name:absent")
			restore := func() {}
			if kind == "sqlite" {
				if runtime.GOOS == "windows" {
					journal := filepath.Join(root, "registrations.db-journal")
					if err := os.Mkdir(journal, 0700); err != nil {
						t.Fatal("obstruct journal")
					}
					restore = func() {
						if err := os.Remove(journal); err != nil {
							t.Error("clear journal obstruction")
						}
					}
				} else {
					if err := os.Chmod(root, 0500); err != nil {
						t.Fatal("chmod")
					}
					restore = func() {
						if err := os.Chmod(root, 0700); err != nil {
							t.Error("restore permissions")
						}
					}
				}
				t.Cleanup(func() { restore() })
			}
			removed, err := s.RemoveDetachedSessions(t.Context(), selectors)
			if err == nil || len(removed) != 0 || len(s.byRegistration) != 4 {
				t.Fatal("failed batch lost rows")
			}
			fail = false
			restore()
			restore = func() {}
			removed, err = s.RemoveDetachedSessions(t.Context(), selectors)
			if err != nil || len(removed) != 2 || len(s.byRegistration) != 2 || calls != 2 {
				t.Fatal("batch retry did not remove exactly the eligible rows")
			}
			if _, err = s.GetSession(t.Context(), selectors[2]); err != nil {
				t.Fatal("live row lost")
			}
			if err := s.Close(); err != nil {
				t.Fatal("close")
			}
			reopened, err := NewSQLitePersistentRemoteService(root)
			if err != nil {
				t.Fatal("reopen")
			}
			defer reopened.Close()
			if len(reopened.byRegistration) != 2 {
				t.Fatal("SQLite batch did not persist exact removals")
			}
		})
	}
}
