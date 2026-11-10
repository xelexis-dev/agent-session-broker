//go:build linux

package channel

import (
	"os"
	"testing"
)

func TestLinuxClaimProcessStartStable(t *testing.T) {
	first, err := codexClaimProcessStart(os.Getpid())
	if err != nil {
		t.Fatal("first native process start", err)
	}
	for i := 0; i < 20; i++ {
		next, err := codexClaimProcessStart(os.Getpid())
		if err != nil {
			t.Fatal("next native process start", err)
		}
		if !first.Equal(next) {
			t.Fatal("process birth changed across real /proc reads")
		}
	}
}
