//go:build windows

package localactivity

import (
	"context"
	"fmt"
	"golang.org/x/sys/windows"
	"os"
)

func nativeProcess(ctx context.Context, pid int) (Process, error) {
	if ctx.Err() != nil || pid <= 0 {
		return Process{}, unavailable
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return Process{}, unavailable
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return Process{}, unavailable
	}
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil || code != 259 {
		return Process{}, unavailable
	}
	host, err := os.Hostname()
	if err != nil {
		return Process{}, unavailable
	}
	return Process{Key: fmt.Sprintf("windows:%d:%d", created.HighDateTime, created.LowDateTime), Host: host, Platform: "windows"}, nil
}
