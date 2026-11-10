//go:build (!windows && !linux && !darwin) || android

package groups

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestNewRegistryRejectsUnsupportedBuildTarget(t *testing.T) {
	registry, err := NewRegistry(t.TempDir())
	if registry != nil {
		t.Fatalf("NewRegistry() registry = %T, want nil", registry)
	}
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), runtime.GOOS) {
		t.Fatalf("NewRegistry() error = %v, want unsupported-platform ErrInvalidInput", err)
	}
}

func TestUnsupportedDirectoryPinFailsClosed(t *testing.T) {
	pin, err := newDirectoryPin("", "", nil)
	if pin != nil {
		t.Fatalf("newDirectoryPin() pin = %T, want nil", pin)
	}
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), runtime.GOOS) || !strings.Contains(err.Error(), "Windows and Linux") {
		t.Fatalf("newDirectoryPin() error = %v, want platform support boundary", err)
	}
}
