//go:build darwin

package accesslog

import (
	"golang.org/x/sys/unix"
	"os"
)

func renameExclusive(dir *os.File, from, to string) error {
	return unix.RenameatxNp(int(dir.Fd()), from, int(dir.Fd()), to, unix.RENAME_EXCL)
}
