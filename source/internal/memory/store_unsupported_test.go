//go:build (!windows && !linux && !darwin) || android

package memory

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestNewStoreRejectsUnsupportedPlatform(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if store != nil {
		t.Fatalf("NewStore() store = %T, want nil", store)
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("NewStore() error = %v, want ErrInvalidInput", err)
	}
	if !strings.Contains(err.Error(), runtime.GOOS) || !strings.Contains(err.Error(), "Windows and Linux") {
		t.Fatalf("NewStore() error = %q, want platform and support boundary", err)
	}
}
