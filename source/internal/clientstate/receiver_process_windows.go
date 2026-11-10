//go:build windows

package clientstate

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"unsafe"
)

func receiverProcessIdentity(ctx context.Context, pid int) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return "", os.ErrNotExist
	}
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err = windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return "", err
	}
	var code uint32
	if err = windows.GetExitCodeProcess(h, &code); err != nil {
		return "", err
	}
	if code != 259 {
		return "", os.ErrNotExist
	}
	var boot struct {
		ID       windows.GUID
		Firmware uint32
		Flags    uint64
	}
	var size uint32
	if err = windows.NtQuerySystemInformation(windows.SystemBootEnvironmentInformation, unsafe.Pointer(&boot), uint32(unsafe.Sizeof(boot)), &size); err != nil {
		return "", err
	}
	return fmt.Sprintf("%v:%d:%d", boot.ID, created.HighDateTime, created.LowDateTime), nil
}
