//go:build android

package memory

import (
	"errors"
	"strings"
	"testing"
)

func TestAndroidDirectoryPinFailsClosed(t *testing.T) {
	pin, err := openPathguardPin("", "", nil)
	if pin != nil {
		t.Fatalf("openPathguardPin() pin = %T, want nil", pin)
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("openPathguardPin() error = %v, want ErrInvalidInput", err)
	}
	if !strings.Contains(err.Error(), "android") {
		t.Fatalf("openPathguardPin() error = %q, want android platform", err)
	}
}
