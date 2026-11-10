package clientstate

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRound2StatePathsSpacesUnicodeLongAndSymlink(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"state with spaces", "한글 상태 폴더", filepath.Join(strings.Repeat("long", 30), strings.Repeat("nested", 25), strings.Repeat("directory", 15))} {
		t.Run(name, func(t *testing.T) {
			s, err := Open(filepath.Join(base, name))
			if err != nil {
				t.Fatal("supported state path rejected")
			}
			cid, err := s.ClientID()
			if err != nil {
				t.Fatal("client id could not be persisted")
			}
			key := SessionKey("scope", "claude", "synthetic-session", cid)
			saved := SessionState{Scope: "scope", ClientID: cid, Registration: Registration{Secret: NewSecret(), RegistrationID: "AAAAAAAAAAAAAAAAAAAAAAAAAA", Revision: 1}}
			if err := s.SaveSession(key, saved); err != nil {
				t.Fatal("state write failed")
			}
			got, found, err := s.LoadSession(key)
			if err != nil || !found || got.Registration != saved.Registration {
				t.Fatal("state round-trip failed")
			}
		})
	}
	if runtime.GOOS == "windows" {
		t.Skip("symlink requires Windows privilege")
	}
	real := filepath.Join(base, "real")
	s, err := Open(real)
	if err != nil {
		t.Fatal("real directory failed")
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal("symlink setup failed")
	}
	linked, err := Open(alias)
	if err != nil {
		t.Fatal("symlinked state open failed")
	}
	first, err := s.ClientID()
	if err != nil {
		t.Fatal("first id failed")
	}
	second, err := linked.ClientID()
	if err != nil || first != second {
		t.Fatal("symlink changed client identity")
	}
	key := SessionKey("scope", "claude", "synthetic-session", first)
	release, err := s.LockSession(key)
	if err != nil {
		t.Fatal("real lock failed")
	}
	defer release()
	if other, err := linked.LockSession(key); !errors.Is(err, ErrLocked) {
		if other != nil {
			other()
		}
		t.Fatal("symlink bypassed ownership lock")
	}
}
func TestRound2OverlongStatePathFailsWithoutPathEcho(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), strings.Repeat("synthetic-private", 80)))
	if err == nil || strings.Contains(err.Error(), "synthetic-private") {
		t.Fatal("overlong path accepted or disclosed")
	}
}
