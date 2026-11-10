//go:build darwin || (linux && !android) || windows

package pathguard

import (
	"errors"
	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPinNativeValidationAndClose(t *testing.T) {
	for _, labels := range []PinLabels{{Store: "membership", Access: "membership"}, {Store: "memory", Access: "nodes"}} {
		t.Run(labels.Store, func(t *testing.T) {
			root := t.TempDir()
			a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
			for _, p := range []string{a, b} {
				if err := os.Mkdir(p, 0700); err != nil {
					t.Fatal(err)
				}
			}
			ai, _, err := InspectDirectory(a)
			if err != nil {
				t.Fatal(err)
			}
			bi, _, err := InspectDirectory(b)
			if err != nil {
				t.Fatal(err)
			}
			pin, err := NewPin(a, a, ai, labels)
			if err != nil {
				t.Fatal(err)
			}
			if err = pin.Validate(); err != nil {
				t.Fatal(err)
			}
			opened, err := pfs.Stat(pin.Path())
			if err != nil || !os.SameFile(ai, opened) {
				t.Fatalf("Path: %v", err)
			}
			if err = pin.Close(); err != nil {
				t.Fatal(err)
			}
			if err = pin.Validate(); err == nil {
				t.Fatalf("closed: %v", err)
			}
			if _, err = NewPin(a, a, bi, labels); err == nil || err.Error() != "pinned "+labels.Store+" directory identity changed" {
				t.Fatalf("identity: %v", err)
			}
			if _, err = NewPin(b, a, ai, labels); err == nil || err.Error() != "pinned "+labels.Store+" directory no longer matches canonical path" {
				t.Fatalf("canonical: %v", err)
			}
			_, err = NewPin(a, filepath.Join(root, "missing"), ai, labels)
			prefix := "open pinned " + labels.Store + " directory: "
			if runtime.GOOS == "darwin" {
				prefix = "open pinned directory: "
			}
			if !errors.Is(err, os.ErrNotExist) || err.Error() != prefix+errors.Unwrap(err).Error() {
				t.Fatalf("open: %v", err)
			}
		})
	}
}
