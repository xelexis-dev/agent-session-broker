//go:build linux && !android

package attachmentclient

import (
	"golang.org/x/sys/unix"
	"os"
)

func renameAbsent(old, new string) error {
	return unix.Renameat2(unix.AT_FDCWD, old, unix.AT_FDCWD, new, unix.RENAME_NOREPLACE)
}
func createPart(path string) (*os.File, error) {
	return os.OpenFile(path, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0600)
}
