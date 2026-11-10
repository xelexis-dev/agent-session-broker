//go:build darwin

package localactivity

import (
	"context"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

// No argv, environment, executable path, or external process is read here.
func nativeProcess(ctx context.Context, pid int) (Process, error) {
	if ctx.Err() != nil || pid <= 0 {
		return Process{}, unavailable
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || info.Proc.P_pid != int32(pid) || info.Proc.P_stat == 0 || info.Proc.P_stat == 5 {
		return Process{}, unavailable
	}
	host, err := os.Hostname()
	if err != nil {
		return Process{}, unavailable
	}
	started := info.Proc.P_starttime
	// Claude's procStart encoding is unverified on this platform. An empty
	// RecordStart deliberately prevents a Claude file from being trusted.
	return Process{Key: fmt.Sprintf("darwin:%d:%d", started.Sec, started.Usec), Host: host, Platform: "darwin"}, nil
}
