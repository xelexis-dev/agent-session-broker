//go:build darwin || linux

package channel

import (
	"os"

	"golang.org/x/sys/unix"
)

// openTokenFile opens without blocking (a FIFO with no writer must not hang startup) and checks the same descriptor.
func openTokenFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		f.Close()
		return nil, err
	}
	switch {
	case uint32(st.Mode)&unix.S_IFMT != unix.S_IFREG:
		err = tokenFileNotRegular
	case uint32(st.Mode)&0o077 != 0:
		err = tokenFileShared
	case int(st.Uid) != os.Getuid():
		err = tokenFileForeign
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
