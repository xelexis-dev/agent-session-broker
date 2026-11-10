package channel

import (
	"os"
	"runtime"
	"testing"
)

// Unix socket paths must stay short even when CI supplies a deep TMPDIR.
func shortSocketDir(t *testing.T, prefix string) string {
	t.Helper()
	base := "/tmp"
	if runtime.GOOS == "darwin" {
		base = "/private/tmp"
	}
	dir, err := os.MkdirTemp(base, prefix)
	if err != nil {
		t.Fatal("create short socket directory:", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal("protect short socket directory:", err)
	}
	return dir
}

func TestShortSocketDirIgnoresLongTempRootAndCleansUp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket fixture")
	}
	t.Setenv("TMPDIR", t.TempDir()+"/deep/not-created/temp/root")
	var dir string
	t.Run("private-directory", func(t *testing.T) {
		dir = shortSocketDir(t, "asb-sock-")
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatal("socket directory is not private")
		}
		if len(dir)+len("/i.sock") >= 104 {
			t.Fatal("socket path exceeds portable Unix limit")
		}
	})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("socket directory cleanup did not run")
	}
}
