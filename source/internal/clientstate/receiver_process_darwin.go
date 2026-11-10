//go:build darwin

package clientstate

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

func receiverProcessIdentity(ctx context.Context, pid int) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	// The singular helper converts a successful zero-length lookup into EIO.
	// A slice preserves absence without treating actual syscall errors as death.
	rows, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if errors.Is(err, unix.ESRCH) {
		return "", os.ErrNotExist
	}
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", os.ErrNotExist
	}
	if len(rows) != 1 {
		return "", errDeviceIdentity
	}
	p := rows[0]
	if p.Proc.P_pid != int32(pid) || p.Proc.P_stat == 0 || p.Proc.P_stat == 5 {
		return "", os.ErrNotExist
	}
	boot, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d:%d:%d", boot.Sec, boot.Usec, p.Proc.P_starttime.Sec, p.Proc.P_starttime.Usec), nil
}
