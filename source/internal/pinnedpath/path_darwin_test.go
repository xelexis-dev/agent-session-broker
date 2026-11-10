package pinnedpath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOperationsStayInPinnedDirectoryAfterReplacement(t *testing.T) {
	base := t.TempDir()
	original := filepath.Join(base, "original")
	moved := filepath.Join(base, "moved")
	outside := t.TempDir()
	if err := os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	pin, err := Open(original)
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	path := DescriptorPath(pin)
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, original); err != nil {
		t.Fatal(err)
	}
	file, err := CreateTemp(path, "probe-*")
	if err != nil {
		t.Fatal(err)
	}
	name := file.Name()
	if _, err := file.WriteString("pinned"); err != nil {
		t.Fatal(err)
	}
	file.Close()
	linked := filepath.Join(path, "linked")
	if err := Link(name, linked); err != nil {
		t.Fatal(err)
	}
	if err := Link(name, linked); !os.IsExist(err) {
		t.Fatalf("duplicate link: %v", err)
	}
	renamed := filepath.Join(path, "renamed")
	if err := Rename(linked, renamed); err != nil {
		t.Fatal(err)
	}
	a, err := Lstat(name)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Stat(renamed)
	if err != nil || !os.SameFile(a, b) {
		t.Fatalf("identity: %v", err)
	}
	resolved, err := EvalSymlinks(path)
	expected, _ := filepath.EvalSymlinks(moved)
	if err != nil || resolved != expected {
		t.Fatalf("resolve %q != %q: %v", resolved, expected, err)
	}
	if err := Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := Remove(renamed); err != nil {
		t.Fatal(err)
	}
	if err := Mkdir(filepath.Join(path, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := Remove(filepath.Join(path, "child")); err != nil {
		t.Fatal(err)
	}
	entries, err := ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside modified: %v %v", entries, err)
	}
	entries, err = ReadDir(path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cleanup: %v %v", entries, err)
	}
}

func TestPinnedOpenRejectsSymlinkAndTraversal(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	pin, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	path := DescriptorPath(pin)
	info, err := Lstat(filepath.Join(path, "link"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("lstat link: %v", err)
	}
	if f, err := Open(filepath.Join(path, "link")); err == nil {
		f.Close()
		t.Fatal("followed symlink")
	}
	if f, err := Open(path + "/../secret"); err == nil {
		f.Close()
		t.Fatal("accepted traversal")
	}
}
