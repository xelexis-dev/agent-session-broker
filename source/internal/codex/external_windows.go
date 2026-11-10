//go:build windows

package codex

import (
	"context"
	"fmt"
	"golang.org/x/sys/windows"
	"path/filepath"
	"strings"
)

func externalProcessKey(ctx context.Context, pid int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	var exit uint32
	if err := windows.GetExitCodeProcess(h, &exit); err != nil || exit != 259 {
		return "", ErrSessionNotFound
	}
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(h, 0, &buffer[0], &size); err != nil {
		return "", err
	}
	path := windows.UTF16ToString(buffer[:size])
	if !strings.EqualFold(filepath.Base(path), "codex.exe") {
		return "", ErrSessionNotFound
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d:%s", created.HighDateTime, created.LowDateTime, strings.ToLower(path)), nil
}
