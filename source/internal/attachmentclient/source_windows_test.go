//go:build windows

package attachmentclient

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func junction(t *testing.T, target, path string) {
	t.Helper()
	if e := exec.Command("cmd", "/c", "mklink", "/J", path, target).Run(); e != nil {
		t.Fatal("synthetic junction create failed")
	}
}
func TestSourceWindowsFinalPaths(t *testing.T) {
	root := realTemp(t)
	outside := realTemp(t)
	put(t, filepath.Join(root, "ok.bin"), []byte("synthetic"))
	put(t, filepath.Join(outside, "secret.bin"), []byte("synthetic"))
	junction(t, outside, filepath.Join(root, "below"))
	code(t, root, "below/secret.bin", "", "attachment-link")
	for _, name := range []string{"ok.bin:stream", "ok.bin.", "ok.bin ", "NUL.txt", "CON", "COM1", "LPT9.bin"} {
		code(t, root, name, "", "attachment-outside-project")
	}
	if e := os.Link(filepath.Join(root, "ok.bin"), filepath.Join(root, "hard.bin")); e != nil {
		t.Fatal("hardlink create")
	}
	code(t, root, "hard.bin", "", "attachment-link")
}

func TestSourceWindowsShortAlias(t *testing.T) {
	root := realTemp(t)
	put(t, filepath.Join(root, "credentials.json"), []byte("synthetic"))
	p, _ := windows.UTF16PtrFromString(filepath.Join(root, "credentials.json"))
	b := make([]uint16, 32768)
	n, e := windows.GetShortPathName(p, &b[0], uint32(len(b)))
	if e != nil {
		t.Fatal("short path query")
	}
	short := filepath.Base(windows.UTF16ToString(b[:n]))
	if strings.EqualFold(short, "credentials.json") {
		t.Skip("8.3 environment-unavailable: GetShortPathName returned the long fixture name")
	}
	code(t, root, short, "", "attachment-credential")
}

func TestSourceWindowsJunctionAboveCWD(t *testing.T) {
	root := realTemp(t)
	put(t, filepath.Join(root, "ok.bin"), []byte("synthetic"))
	target, e := os.Open(root)
	if e != nil {
		t.Fatal("junction fixture target cannot open")
	}
	defer target.Close()
	expected, e := target.Stat()
	if e != nil || !expected.IsDir() {
		t.Fatal("junction fixture target is not a directory")
	}
	alias := filepath.Join(realTemp(t), "above")
	junction(t, root, alias)
	name, e := windows.UTF16PtrFromString(alias)
	if e != nil {
		t.Fatal("junction fixture name")
	}
	attributes, e := windows.GetFileAttributes(name)
	if e != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		t.Fatal("junction fixture was not created")
	}
	probe, e := os.Open(alias)
	if errors.Is(e, syscall.Errno(448)) {
		t.Skip("above-CWD acceptance UNVERIFIED (RedirectionGuard 448)")
	}
	if e != nil {
		t.Fatal("junction fixture OS preflight failed")
	}
	opened, e := probe.Stat()
	probe.Close()
	if e != nil || !os.SameFile(expected, opened) {
		t.Fatal("junction fixture resolves to a different directory")
	}
	source, e := OpenSource(alias, "ok.bin", "")
	if e != nil {
		t.Fatal("valid junction above CWD refused")
	}
	source.Close()
}
