//go:build darwin

package pathguard

import (
	"fmt"
	"os"

	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
	"golang.org/x/sys/unix"
)

func NewPin(canonicalPath, operationPath string, identity os.FileInfo, labels PinLabels) (Pin, error) {
	file, err := pfs.OpenFile(operationPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open pinned directory: %w", err)
	}
	pin := &directoryPin{
		file:          file,
		operationPath: pfs.DescriptorPath(file),
		canonicalPath: canonicalPath,
		identity:      identity,
		labels:        labels,
	}
	if err := pin.Validate(); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := pfs.Stat(pin.operationPath); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("access pinned %s directory through directory descriptor: %w", labels.Access, err)
	}
	return pin, nil
}
