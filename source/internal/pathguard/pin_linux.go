//go:build linux && !android

package pathguard

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func NewPin(canonicalPath, operationPath string, identity os.FileInfo, labels PinLabels) (Pin, error) {
	fd, err := unix.Open(operationPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open pinned %s directory: %w", labels.Store, err)
	}
	file := os.NewFile(uintptr(fd), operationPath)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open pinned %s directory: invalid descriptor", labels.Store)
	}
	pin := &directoryPin{
		file:          file,
		operationPath: fmt.Sprintf("/proc/self/fd/%d", fd),
		canonicalPath: canonicalPath,
		identity:      identity,
		labels:        labels,
	}
	if err := pin.Validate(); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := os.Stat(pin.operationPath); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("access pinned %s directory through procfs: %w", labels.Access, err)
	}
	return pin, nil
}
