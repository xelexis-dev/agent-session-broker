//go:build darwin || (linux && !android)

package attachmentclient

import (
	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
)

func canonicalCWD(path string) (string, error) { return filepath.EvalSymlinks(path) }
func openLeaf(path string) (*os.File, error) {
	return pfs.OpenFile(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
}
func validateLeaf(_ *os.File, info os.FileInfo, _, _ string) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return failure("attachment-link")
	}
	if !info.Mode().IsRegular() {
		return failure("attachment-not-file")
	}
	return nil
}

func rootLocation(file *os.File) (string, error) {
	prefix := "/proc/self/fd/"
	if runtime.GOOS == "darwin" {
		prefix = "/dev/fd/"
	}
	return pfs.EvalSymlinks(prefix + strconv.FormatUint(uint64(file.Fd()), 10))
}
