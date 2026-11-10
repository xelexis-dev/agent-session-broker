//go:build !windows

package clientstate

import (
	"os"
	"syscall"
)

func protectSteerPath(string) error { return nil }
func privateSteerPath(_ string, info os.FileInfo, strict bool) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) {
		return false
	}
	if !info.IsDir() && st.Nlink != 1 {
		return false
	}
	if !strict {
		return info.Mode().Perm()&0022 == 0
	}
	mode := os.FileMode(0600)
	if info.IsDir() {
		// Read-only private directories still allow watermark reconciliation.
		return info.Mode().Perm()&0077 == 0 && info.Mode().Perm()&0500 == 0500
	}
	return info.Mode().Perm() == mode
}

// Unlike lockFile, GC never creates a missing lock and must inspect its held fd.
func lockExistingSteerFile(path string) (*os.File, func(), error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, nil, ErrSteerRecovery
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, nil, ErrLocked
	}
	return file, func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}
