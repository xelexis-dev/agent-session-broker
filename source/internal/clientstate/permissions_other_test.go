//go:build !windows

package clientstate

import (
	"os"
	"testing"
)

func readOnlyStateDirectory(t *testing.T, path string) func() {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses read-only mode")
	}
	if err := os.Chmod(path, 0500); err != nil {
		t.Fatal("fixture chmod")
	}
	return func() {
		if err := os.Chmod(path, 0700); err != nil {
			t.Error("fixture restore")
		}
	}
}

func insecureDevicePath(t *testing.T, path string) func() {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal("fixture stat")
	}
	mode := os.FileMode(0644)
	if info.IsDir() {
		mode = 0500
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal("fixture chmod")
	}
	restore := func() {
		if err := os.Chmod(path, info.Mode().Perm()); err != nil {
			t.Error("fixture restore")
		}
	}
	t.Cleanup(restore)
	return restore
}
