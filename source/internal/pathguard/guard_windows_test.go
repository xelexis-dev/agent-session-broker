//go:build windows

package pathguard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestValidateDirectoryPathAcceptsEightDotThreeNameForRealDirectory(t *testing.T) {
	longPath := filepath.Join(t.TempDir(), "PathGuardEightDotThreeAliasComponent")
	if err := os.Mkdir(longPath, 0o700); err != nil {
		t.Fatal(err)
	}
	shortPath := windowsShortPath(t, longPath)
	if filepath.Clean(shortPath) == filepath.Clean(longPath) {
		t.Skip("volume does not provide an 8.3 name for the test directory")
	}

	shortInfo, err := os.Lstat(shortPath)
	if err != nil {
		t.Fatal(err)
	}
	longInfo, err := os.Stat(longPath)
	if err != nil {
		t.Fatal(err)
	}
	if hasReparsePoint(shortInfo) || !os.SameFile(shortInfo, longInfo) {
		t.Fatalf("8.3 path is not a direct name for the test directory: short=%q long=%q", shortPath, longPath)
	}

	identity, _, err := ValidateDirectoryPath(shortPath)
	if err != nil {
		t.Fatalf("ValidateDirectoryPath(%q) error = %v", shortPath, err)
	}
	if !os.SameFile(identity, longInfo) {
		t.Fatalf("ValidateDirectoryPath(%q) identity does not match %q", shortPath, longPath)
	}
}

func TestValidateDirectoryPathRejectsTrailingDotAlias(t *testing.T) {
	longPath := filepath.Join(t.TempDir(), "PathGuardTrailingDotAlias")
	if err := os.Mkdir(longPath, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasPath := longPath + "."
	aliasInfo, err := os.Lstat(aliasPath)
	if err != nil {
		t.Fatal(err)
	}
	longInfo, err := os.Stat(longPath)
	if err != nil {
		t.Fatal(err)
	}
	if hasReparsePoint(aliasInfo) || !os.SameFile(aliasInfo, longInfo) {
		t.Fatalf("trailing-dot path is not a direct alias for the test directory: alias=%q long=%q", aliasPath, longPath)
	}

	if _, _, err := ValidateDirectoryPath(aliasPath); err == nil {
		t.Fatalf("ValidateDirectoryPath(%q) succeeded, want unsafe alias rejection", aliasPath)
	}
}

func TestWalkRejectsAncestorJunctionReplacementBetweenComponents(t *testing.T) {
	base := t.TempDir()
	ancestor := filepath.Join(base, "walk-ancestor")
	child := filepath.Join(ancestor, "child")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	displaced := filepath.Join(base, "walk-displaced")
	swapped := false
	t.Cleanup(func() {
		if !swapped {
			return
		}
		if err := os.Remove(ancestor); err != nil {
			t.Errorf("remove replacement junction: %v", err)
			return
		}
		if err := os.Rename(displaced, ancestor); err != nil {
			t.Errorf("restore displaced ancestor: %v", err)
		}
	})

	_, err := walkToNearestExistingWithHook(child, func(operation string) error {
		same, compareErr := samePath(operation, ancestor)
		if compareErr != nil || !same || swapped {
			return compareErr
		}
		if err := os.Rename(ancestor, displaced); err != nil {
			return err
		}
		output, junctionErr := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", ancestor, displaced).CombinedOutput()
		if junctionErr != nil {
			_ = os.Rename(displaced, ancestor)
			return fmt.Errorf("create replacement junction: %w: %s", junctionErr, output)
		}
		swapped = true
		return nil
	})
	if !swapped {
		t.Fatal("walk hook did not replace the inspected ancestor")
	}
	if err == nil {
		t.Fatal("walk accepted an ancestor replaced by a junction between components")
	}
}

func windowsShortPath(t *testing.T, path string) string {
	t.Helper()
	pathPointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]uint16, syscall.MAX_PATH)
	length, err := syscall.GetShortPathName(pathPointer, &buffer[0], uint32(len(buffer)))
	if err != nil {
		t.Fatal(err)
	}
	if length >= uint32(len(buffer)) {
		buffer = make([]uint16, length+1)
		length, err = syscall.GetShortPathName(pathPointer, &buffer[0], uint32(len(buffer)))
		if err != nil {
			t.Fatal(err)
		}
	}
	return syscall.UTF16ToString(buffer[:length])
}
