//go:build darwin || linux

package channel

import (
	"crypto/rand"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"time"
)

func codexYieldDirectory(path string, create bool) (int, error) {
	dir := filepath.Dir(path)
	if create {
		if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
			return -1, err
		}
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Uid != uint32(os.Getuid()) || st.Mode&0077 != 0 {
		unix.Close(fd)
		return -1, errors.New("unsafe yield directory")
	}
	return fd, nil
}
func publishCodexYield(path string, data []byte) error {
	dir, err := codexYieldDirectory(path, true)
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	tmp := ".yield-" + rand.Text()
	fd, err := unix.Openat(dir, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(dir, tmp, 0)
	f := os.NewFile(uintptr(fd), tmp)
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return unix.Renameat(dir, tmp, dir, filepath.Base(path))
}
func readCodexYield(path string, take bool) ([]byte, time.Time, error) {
	dir, err := codexYieldDirectory(path, false)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer unix.Close(dir)
	name := filepath.Base(path)
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, time.Time{}, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || !safeCodexClaimStat(&st) || st.Nlink != 1 {
		return nil, time.Time{}, errors.New("unsafe yield file")
	}
	info, err := f.Stat()
	if err != nil {
		return nil, time.Time{}, err
	}
	if take {
		taken := name + ".taken-" + rand.Text()
		if err := unix.Renameat(dir, name, dir, taken); err != nil {
			return nil, time.Time{}, err
		}
		defer unix.Unlinkat(dir, taken, 0)
		var moved unix.Stat_t
		if unix.Fstatat(dir, taken, &moved, unix.AT_SYMLINK_NOFOLLOW) != nil || moved.Ino != st.Ino || moved.Dev != st.Dev {
			return nil, time.Time{}, errors.New("yield changed")
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, 1025))
	if len(data) > 1024 {
		err = errors.New("oversize yield")
	}
	return data, info.ModTime(), err
}
func removeCodexYield(path string) {
	dir, err := codexYieldDirectory(path, false)
	if err != nil {
		return
	}
	defer unix.Close(dir)
	_ = unix.Unlinkat(dir, filepath.Base(path), 0)
}
