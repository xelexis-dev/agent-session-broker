//go:build linux || darwin

package attachments

import (
	"golang.org/x/sys/unix"
	"os"
)

func diskCapacity(file *os.File) (Capacity, error) {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &stat); err != nil {
		return Capacity{}, err
	}
	return Capacity{Free: uint64(stat.Bavail) * uint64(stat.Bsize), Total: uint64(stat.Blocks) * uint64(stat.Bsize)}, nil
}
