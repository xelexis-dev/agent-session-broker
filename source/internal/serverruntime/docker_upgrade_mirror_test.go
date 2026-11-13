//go:build !windows

package serverruntime

import (
	"os"
	"testing"
)

func TestDockerUpgradeMirror(t *testing.T) {
	if os.Getenv("ASB_U2_013") == "" || os.Getenv("ASB_U2_015") == "" {
		t.Skip("opt-in disposable Docker upgrade matrix")
	}
	// The old matrix requires0.14.0/0.15.0 images; Release executables remain available.
	t.Fatal("0.14.0/0.15.0 image deleted with the -deploy package (QA72 B); use the executable upgrade matrix")
}
