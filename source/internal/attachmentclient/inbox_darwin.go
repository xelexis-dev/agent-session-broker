//go:build darwin

package attachmentclient

import (
	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
	"golang.org/x/sys/unix"
	"os"
)

func renameAbsent(old, new string) error {
	// /dev/fd paths need descriptor-relative rename to retain the pinned parent.
	a, b, e := splitPin(old)
	if e != nil {
		return e
	}
	c, d, e := splitPin(new)
	if e != nil {
		return e
	}
	return unix.RenameatxNp(a, b, c, d, unix.RENAME_EXCL)
}
func createPart(path string) (*os.File, error) {
	return pfs.OpenFile(path, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0600)
}
