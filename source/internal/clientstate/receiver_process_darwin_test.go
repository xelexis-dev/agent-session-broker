//go:build darwin

package clientstate

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

func TestRound3DarwinReapedReceiverIsAbsent(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRound3ReapedProcessHelper$")
	if err := cmd.Run(); err != nil {
		t.Fatal("fixture child did not exit cleanly")
	}
	if _, err := receiverProcessIdentity(t.Context(), cmd.Process.Pid); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reaped receiver must be absent, got %v", err)
	}
	if key, err := receiverProcessIdentity(t.Context(), os.Getpid()); err != nil || key == "" {
		t.Fatal("live receiver lookup failed")
	}
}
func TestRound3ReapedProcessHelper(t *testing.T) {}
