//go:build darwin || linux

package channel

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openCodexClaim(path string) (*os.File, error) {
	d, e := unix.Open(filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	defer unix.Close(d)
	fd, e := unix.Openat(d, filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || !safeCodexClaimStat(&st) {
		f.Close()
		return nil, errors.New("unsafe claim")
	}
	return f, nil
}

func safeCodexClaimStat(st *unix.Stat_t) bool {
	return st.Uid == uint32(os.Getuid()) && st.Mode&unix.S_IFMT == unix.S_IFREG && st.Mode&07777&^0600 == 0 && st.Size <= 1024
}
