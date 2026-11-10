package attachmentclient

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func realTemp(t *testing.T) string {
	t.Helper()
	path, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	return path
}
func put(t *testing.T, path string, data []byte) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(path, data, 0600); e != nil {
		t.Fatal(e)
	}
}
func code(t *testing.T, root, path, state, want string) {
	t.Helper()
	f, e := OpenSource(root, path, state)
	if f != nil {
		f.Close()
	}
	if e == nil || e.Error() != want {
		t.Fatalf("guard code=%v want %s", e, want)
	}
}
func TestSourceGuard(t *testing.T) {
	root := realTemp(t)
	put(t, filepath.Join(root, "nested", "ok.bin"), []byte{0, 1, 2, 255})
	f, e := OpenSource(root, "nested/ok.bin", "")
	if e != nil {
		t.Fatal(e)
	}
	data, e := io.ReadAll(f)
	f.Close()
	if e != nil || len(data) != 4 {
		t.Fatal("allowed file differs")
	}
	for _, p := range []string{"../outside", "nested/../ok", "nested/../../outside", "a\x00b", "name:stream", "name.", "name ", "CON", "nul.txt", "LPT1.bin"} {
		t.Run(p, func(t *testing.T) { code(t, root, p, "", "attachment-outside-project") })
	}
	for _, p := range []string{filepath.Join(root, "nested", "ok.bin"), `C:\file`, `\\host\share\file`} {
		code(t, root, p, "", "attachment-absolute-path")
	}
	for _, p := range []string{".credentials", "secrets", ".mcp.json", ".codex", ".claude", ".kimi", ".ssh", ".gnupg", ".aws", ".azure", ".kube", ".docker", ".git", ".netrc", "_netrc", ".npmrc", ".pypirc", ".git-credentials", ".env", ".env.local", "id_rsa-backup", "id_dsa", "id_ecdsa", "id_ed25519", "x.pem", "x.key", "x.p12", "x.pfx", "x.ppk", "x.jks", "x.keystore", "x.kdbx", "credentials", "credentials.json", "credentials.yaml", "credentials.yml", "admin-password", "admin-password.txt", "known_hosts", "authorized_keys", ".config/gcloud/config"} {
		mixed := strings.ToUpper(p)
		t.Run(mixed, func(t *testing.T) { code(t, root, mixed, "", "attachment-credential") })
	}
	state := filepath.Join(root, "innocent-state")
	put(t, filepath.Join(state, "payload"), []byte("synthetic"))
	code(t, root, "innocent-state/payload", state, "attachment-credential")
	code(t, root, "nested", "", "attachment-not-file")
	home, _ := os.UserHomeDir()
	code(t, home, "file", "", "attachment-outside-project")
	volume := filepath.VolumeName(root) + string(filepath.Separator)
	code(t, volume, "file", "", "attachment-outside-project")
}
func TestSourceLinksAndSwap(t *testing.T) {
	root := realTemp(t)
	outside := realTemp(t)
	put(t, filepath.Join(root, "plain", "file"), []byte("synthetic"))
	put(t, filepath.Join(outside, "file"), []byte("private-synthetic"))
	put(t, filepath.Join(root, "hard-source"), []byte("synthetic"))
	if e := os.Link(filepath.Join(root, "hard-source"), filepath.Join(root, "hard")); e != nil {
		t.Fatal(e)
	}
	code(t, root, "hard", "", "attachment-link")
	if e := os.Symlink(filepath.Join(outside, "file"), filepath.Join(root, "link")); e != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlink privilege unavailable; junction native cases cover traversal")
		}
		t.Fatal(e)
	}
	code(t, root, "link", "", "attachment-link")
	if e := os.Symlink(outside, filepath.Join(root, "dirlink")); e != nil {
		t.Fatal(e)
	}
	code(t, root, "dirlink/file", "", "attachment-link")
	f, e := openSource(root, "plain/file", "", func() {
		if err := os.Rename(filepath.Join(root, "plain"), filepath.Join(root, "moved")); err != nil {
			return
		}
		if err := os.Symlink(outside, filepath.Join(root, "plain")); err != nil {
			t.Fatal(err)
		}
	})
	if f != nil {
		f.Close()
	}
	if e == nil || e.Error() != "attachment-link" {
		t.Fatal("directory swap was not refused")
	}
}
func TestSourceCanonicalCWDAndSize(t *testing.T) {
	root := realTemp(t)
	put(t, filepath.Join(root, "file"), []byte("synthetic"))
	alias := filepath.Join(realTemp(t), "alias")
	if e := os.Symlink(root, alias); e == nil {
		f, e := OpenSource(alias, "file", "")
		if e != nil {
			t.Fatal(e)
		}
		f.Close()
	}
	for _, size := range []int64{MaxFileBytes, MaxFileBytes + 1} {
		file, e := os.OpenFile(filepath.Join(root, "large"), os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0600)
		if e != nil {
			t.Fatal(e)
		}
		if e = file.Truncate(size); e != nil {
			t.Fatal(e)
		}
		file.Close()
		f, e := OpenSource(root, "large", "")
		if size == MaxFileBytes {
			if e != nil {
				t.Fatal(e)
			}
			f.Close()
		} else if e == nil || e.Error() != "attachment-too-large" {
			if f != nil {
				f.Close()
			}
			t.Fatal("oversize accepted")
		}
	}
}
