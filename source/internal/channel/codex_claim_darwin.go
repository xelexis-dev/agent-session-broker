package channel

import (
	"golang.org/x/sys/unix"
	"os"
	"time"
)

func codexClaimProcessStart(pid int) (time.Time, error) {
	p, e := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if e != nil {
		return time.Time{}, e
	}
	if p.Proc.P_pid != int32(pid) || p.Proc.P_stat == 0 || p.Proc.P_stat == 5 {
		return time.Time{}, os.ErrNotExist
	}
	return time.Unix(p.Proc.P_starttime.Sec, int64(p.Proc.P_starttime.Usec)*1000), nil
}
