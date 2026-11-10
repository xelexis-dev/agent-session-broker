package groups

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDeleteProjectStorageIsScopedAndRecreatable(t *testing.T) {
	root := t.TempDir()
	r, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	a, err := r.ProjectStorage(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.ProjectStorage(context.Background(), "keep")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []ProjectDirectory{a, b} {
		if err = os.WriteFile(filepath.Join(p.CanonicalPath(), "memory.md"), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = r.DeleteProjectStorage(context.Background(), "../keep"); err == nil {
		t.Fatal("unsafe name accepted")
	}
	if err = r.DeleteProjectStorage(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(a.CanonicalPath()); !os.IsNotExist(err) {
		t.Fatalf("project remains: %v", err)
	}
	if _, err = os.Stat(filepath.Join(b.CanonicalPath(), "memory.md")); err != nil {
		t.Fatal(err)
	}
	if err = r.DeleteProjectStorage(context.Background(), "demo"); err != nil {
		t.Fatal("retry missing", err)
	}
	fresh, err := r.ProjectStorage(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(fresh.CanonicalPath(), "memory.md")); !os.IsNotExist(err) {
		t.Fatal("old data survived")
	}
}

func TestDeleteProjectStorageDoesNotFollowOutsideLink(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	r, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err = os.WriteFile(filepath.Join(outside, "keep"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := r.ProjectStorage(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(p.CanonicalPath(), "link")); err != nil {
		t.Skip("symlink unavailable", err)
	}
	if err = r.DeleteProjectStorage(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal("outside file removed", err)
	}
	if err = os.Symlink(outside, filepath.Join(root, projectDirectoryName("fake"))); err != nil {
		t.Fatal(err)
	}
	if err = r.DeleteProjectStorage(context.Background(), "fake"); err == nil {
		t.Fatal("top-level link accepted")
	}
}
