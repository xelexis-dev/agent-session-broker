package nativeinbox

import (
	"context"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

func socketPath(value string) (string, error) {
	path, err := stripSocketScheme(value)
	const prefix = `\\.\pipe\`
	if err != nil || !strings.HasPrefix(path, prefix) {
		return "", ErrUnavailable
	}
	name := strings.TrimPrefix(path, prefix)
	// Claude uses the LOCAL named-pipe namespace on current Windows builds.
	name = strings.TrimPrefix(name, `LOCAL\`)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `\/:`) {
		return "", ErrUnavailable
	}
	return path, nil
}

func dialLocal(ctx context.Context, path string) (writeConnection, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, ErrUnavailable
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Local CreateFile fails immediately while all pipe instances are busy.
		// Overlapped mode lets Go's runtime cancel and deadline-limit writes.
		handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
		if err == nil {
			return os.NewFile(uintptr(handle), "native-inbox"), nil
		}
		if err != windows.ERROR_PIPE_BUSY {
			return nil, ErrDelivery
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
