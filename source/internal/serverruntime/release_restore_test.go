package serverruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"syscall"
	"testing"
)

type releaseFile struct {
	Size   int64
	SHA256 string
}
type releaseManifest map[string]releaseFile

func releaseTree(root string) (releaseManifest, error) {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return nil, errors.New("backup-root")
	}
	out := releaseManifest{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return errors.New("backup-read")
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("backup-nonregular")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return errors.New("backup-read")
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return errors.New("backup-relative")
		}
		sum := sha256.Sum256(raw)
		out[filepath.ToSlash(rel)] = releaseFile{int64(len(raw)), hex.EncodeToString(sum[:])}
		return nil
	})
	return out, err
}

func releaseRestore(source, dest string, manifest releaseManifest) error {
	got, err := releaseTree(source)
	if err != nil || !reflect.DeepEqual(got, manifest) {
		return errors.New("backup-integrity")
	}
	if info, err := os.Lstat(dest); err == nil {
		if !info.IsDir() {
			return errors.New("restore-not-empty")
		}
		entries, err := os.ReadDir(dest)
		if err != nil || len(entries) != 0 {
			return errors.New("restore-not-empty")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("restore-destination")
	}
	if err := os.MkdirAll(dest, 0700); err != nil {
		return errors.New("restore-directory")
	}
	for name := range manifest {
		raw, err := os.ReadFile(filepath.Join(source, filepath.FromSlash(name)))
		if err != nil {
			return errors.New("restore-read")
		}
		path := filepath.Join(dest, filepath.FromSlash(name))
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return errors.New("restore-directory")
		}
		stream, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return errors.New("restore-create")
		}
		_, writeErr := stream.Write(raw)
		closeErr := stream.Close()
		if writeErr != nil || closeErr != nil {
			return errors.New("restore-write")
		}
	}
	got, err = releaseTree(dest)
	if err != nil || !reflect.DeepEqual(got, manifest) {
		return errors.New("restore-integrity")
	}
	return nil
}

func releaseBackup(server *upgradeServer, dest string) (releaseManifest, error) {
	if server.cmd != nil {
		return nil, errors.New("backup-server-running")
	}
	manifest, err := releaseTree(server.dir)
	if err != nil {
		return nil, err
	}
	if err = releaseRestore(server.dir, dest, manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

func TestReleaseRestoreRejectsTamperedBackup(t *testing.T) {
	source, dest := t.TempDir(), filepath.Join(t.TempDir(), "restore")
	if os.WriteFile(filepath.Join(source, "control.db"), []byte("synthetic-before"), 0600) != nil {
		t.Fatal("fixture-write")
	}
	manifest, err := releaseTree(source)
	if err != nil {
		t.Fatal("fixture-manifest")
	}
	if os.WriteFile(filepath.Join(source, "control.db"), []byte("synthetic-tampered"), 0600) != nil {
		t.Fatal("fixture-tamper")
	}
	if releaseRestore(source, dest, manifest) == nil {
		t.Fatal("tampered backup accepted before server start")
	}
}

func TestReleaseBackupRestoreBoundaries(t *testing.T) {
	source := t.TempDir()
	if os.Mkdir(filepath.Join(source, "memory"), 0700) != nil {
		t.Fatal("fixture-directory")
	}
	if os.WriteFile(filepath.Join(source, "memory", "node.md"), []byte("synthetic-memory"), 0600) != nil {
		t.Fatal("fixture-write")
	}
	backup := filepath.Join(t.TempDir(), "backup")
	server := &upgradeServer{dir: source}
	manifest, err := releaseBackup(server, backup)
	if err != nil {
		t.Fatal("backup-stopped")
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if releaseRestore(backup, restored, manifest) != nil {
		t.Fatal("restore-valid")
	}
	if releaseRestore(backup, restored, manifest) == nil {
		t.Fatal("restore-nonempty-accepted")
	}
	for _, change := range []string{"missing", "extra", "link"} {
		t.Run(change, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "copy")
			if releaseRestore(backup, root, manifest) != nil {
				t.Fatal("fixture-copy")
			}
			switch change {
			case "missing":
				if os.Remove(filepath.Join(root, "memory", "node.md")) != nil {
					t.Fatal("fixture-remove")
				}
			case "extra":
				if os.WriteFile(filepath.Join(root, "extra"), []byte("extra"), 0600) != nil {
					t.Fatal("fixture-write")
				}
			case "link":
				if err := os.Symlink(source, filepath.Join(root, "link")); err != nil {
					if releaseSymlinkPrivilegeMissing(runtime.GOOS, err) {
						t.Skip("Windows symlink creation requires Developer Mode or the symbolic-link privilege")
					}
					t.Fatalf("fixture-symlink: %v", err)
				}
			}
			if releaseRestore(root, filepath.Join(t.TempDir(), "restore"), manifest) == nil {
				t.Fatal("altered-backup-accepted")
			}
		})
	}
}

func releaseSymlinkPrivilegeMissing(goos string, err error) bool {
	// ERROR_PRIVILEGE_NOT_HELD is specific; generic permission and I/O failures remain failures.
	return goos == "windows" && errors.Is(err, syscall.Errno(1314))
}
func TestReleaseSymlinkPrivilegeMissing(t *testing.T) {
	for _, tc := range []struct {
		goos string
		err  error
		want bool
	}{
		{"windows", &os.LinkError{Op: "symlink", Old: "source", New: "link", Err: syscall.Errno(1314)}, true},
		{"windows", syscall.EPERM, false}, {"windows", syscall.EACCES, false},
		{"windows", os.ErrNotExist, false}, {"windows", nil, false},
		{"darwin", syscall.Errno(1314), false}, {"darwin", syscall.EPERM, false},
	} {
		if got := releaseSymlinkPrivilegeMissing(tc.goos, tc.err); got != tc.want {
			t.Errorf("%s %v: skip=%v want=%v", tc.goos, tc.err, got, tc.want)
		}
	}
}
