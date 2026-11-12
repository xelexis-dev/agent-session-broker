//go:build darwin || linux

package accesslog

import (
	"golang.org/x/sys/unix"
	"os"
)

func openLogFile(root *os.Root, dir *os.File, name string, create bool) (*os.File, error) {
	flags := os.O_WRONLY | os.O_APPEND | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if create {
		flags |= os.O_CREATE | os.O_EXCL
	}
	return root.OpenFile(name, flags, 0600)
}
func regular(info os.FileInfo) bool { return info.Mode().IsRegular() }
