//go:build darwin

package broker

import (
	"errors"
	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
	"golang.org/x/sys/unix"
	"os"
	"strings"
)

// Pin the registration child itself: Darwin descriptor operations accept only
// one relative component, and the caller owns the parent descriptor lifetime.
func openRegistrationRoot(root string) (string, func() error, error) {
	if strings.HasPrefix(root, "/dev/fd/") {
		if err := pfs.Mkdir(root, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", nil, err
		}
	} else if err := os.MkdirAll(root, 0700); err != nil {
		return "", nil, err
	}
	file, err := pfs.OpenFile(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", nil, err
	}
	return pfs.DescriptorPath(file), file.Close, nil
}
