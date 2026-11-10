//go:build darwin || (linux && !android)

package attachmentclient

import (
	"errors"
	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
	"golang.org/x/sys/unix"
	"os"
	"strconv"
	"strings"
)

func createLock(path string) (*os.File, error) {
	return pfs.OpenFile(path, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
}
func lockFile(f *os.File) (func(), error) {
	if e := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		return nil, e
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }, nil
}
func diskFull(e error) bool { return errors.Is(e, unix.ENOSPC) }
func splitPin(path string) (int, string, error) {
	parts := strings.Split(strings.TrimPrefix(path, "/dev/fd/"), "/")
	if len(parts) != 2 {
		return 0, "", os.ErrInvalid
	}
	fd, e := strconv.Atoi(parts[0])
	return fd, parts[1], e
}
