//go:build windows

package claude

import (
	"os/exec"
	"testing"
)

func TestWindowsProcessLivenessDetectsForcedExit(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/d", "/c", "pause")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	probe := defaultLivenessProbe{}
	alive, err := probe.Alive(pid)
	if err != nil || !alive {
		t.Fatalf("running process: alive=%v err=%v", alive, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()
	alive, err = probe.Alive(pid)
	if err != nil || alive {
		t.Fatalf("exited process: alive=%v err=%v", alive, err)
	}
}
