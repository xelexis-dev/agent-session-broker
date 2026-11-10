//go:build darwin

package pinnedpath

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// DescriptorPath is meaningful only while file remains open. Callers own its
// lifetime and serialize Close against operations, just as with procfs pins.
func DescriptorPath(file *os.File) string { return fmt.Sprintf("/dev/fd/%d", file.Fd()) }

// A capability accepts only itself or a single child. Nested traversal must
// first pin the next directory, so no intermediate symlink can redirect I/O.
func relative(path string) (int, string, bool, error) {
	const prefix = "/dev/fd/"
	if !strings.HasPrefix(path, prefix) {
		return unix.AT_FDCWD, path, false, nil
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	fd, err := strconv.Atoi(parts[0])
	if err != nil || fd < 0 || len(parts) > 2 {
		return 0, "", true, os.ErrInvalid
	}
	name := "."
	if len(parts) == 2 {
		name = parts[1]
		if name == "" || name == "." || name == ".." {
			return 0, "", true, os.ErrInvalid
		}
	}
	return fd, name, true, nil
}

func Open(path string) (*os.File, error) { return OpenFile(path, os.O_RDONLY, 0) }

func OpenFile(path string, flags int, mode os.FileMode) (*os.File, error) {
	fd, name, pinned, err := relative(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if !pinned {
		return os.OpenFile(path, flags, mode)
	}
	child, err := unix.Openat(fd, name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, uint32(mode.Perm()))
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: path, Err: err}
	}
	return os.NewFile(uintptr(child), path), nil
}

func Stat(path string) (os.FileInfo, error)  { return stat(path, false) }
func Lstat(path string) (os.FileInfo, error) { return stat(path, true) }
func stat(path string, nofollow bool) (os.FileInfo, error) {
	fd, name, pinned, err := relative(path)
	if err != nil {
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if !pinned {
		if nofollow {
			return os.Lstat(path)
		}
		return os.Stat(path)
	}
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK | unix.O_NOFOLLOW
	if nofollow {
		// O_SYMLINK opens the link itself; O_NOFOLLOW would reject it with
		// ELOOP instead. There are no intermediate components in a pin path.
		flags = (flags &^ unix.O_NOFOLLOW) | unix.O_SYMLINK
	}
	child, err := unix.Openat(fd, name, flags, 0)
	if err != nil {
		return nil, &os.PathError{Op: "statat", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(child), path)
	defer file.Close()
	return file.Stat()
}

func ReadDir(path string) ([]os.DirEntry, error) {
	file, err := Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entries, err := file.ReadDir(-1)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, err
}

func CreateTemp(dir, pattern string) (*os.File, error) {
	_, _, pinned, err := relative(dir)
	if err != nil {
		return nil, err
	}
	if !pinned {
		return os.CreateTemp(dir, pattern)
	}
	if strings.ContainsAny(pattern, `/\`) {
		return nil, os.ErrInvalid
	}
	for i := 0; i < 100; i++ {
		suffix := rand.Text()
		name := pattern + suffix
		if at := strings.LastIndexByte(pattern, '*'); at >= 0 {
			name = pattern[:at] + suffix + pattern[at+1:]
		}
		file, err := OpenFile(filepath.Join(dir, name), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if !errors.Is(err, os.ErrExist) {
			return file, err
		}
	}
	return nil, os.ErrExist
}

func Link(oldpath, newpath string) error {
	return twoPaths("linkat", oldpath, newpath, func(a int, b string, c int, d string) error { return unix.Linkat(a, b, c, d, 0) })
}
func Rename(oldpath, newpath string) error {
	return twoPaths("renameat", oldpath, newpath, unix.Renameat)
}
func twoPaths(op, oldpath, newpath string, call func(int, string, int, string) error) error {
	a, b, pinnedA, err := relative(oldpath)
	if err != nil {
		return err
	}
	c, d, pinnedB, err := relative(newpath)
	if err != nil {
		return err
	}
	if !pinnedA && !pinnedB {
		if op == "linkat" {
			return os.Link(oldpath, newpath)
		}
		return os.Rename(oldpath, newpath)
	}
	if !pinnedA || !pinnedB || b == "." || d == "." {
		return os.ErrInvalid
	}
	if err := call(a, b, c, d); err != nil {
		return &os.LinkError{Op: op, Old: oldpath, New: newpath, Err: err}
	}
	return nil
}

func Remove(path string) error {
	fd, name, pinned, err := relative(path)
	if err != nil {
		return err
	}
	if !pinned {
		return os.Remove(path)
	}
	if name == "." {
		return os.ErrInvalid
	}
	err = unix.Unlinkat(fd, name, 0)
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EISDIR) {
		err = unix.Unlinkat(fd, name, unix.AT_REMOVEDIR)
	}
	if err != nil {
		return &os.PathError{Op: "unlinkat", Path: path, Err: err}
	}
	return nil
}

func Mkdir(path string, mode os.FileMode) error {
	fd, name, pinned, err := relative(path)
	if err != nil {
		return err
	}
	if !pinned {
		return os.Mkdir(path, mode)
	}
	if name == "." {
		return os.ErrInvalid
	}
	if err := unix.Mkdirat(fd, name, uint32(mode.Perm())); err != nil {
		return &os.PathError{Op: "mkdirat", Path: path, Err: err}
	}
	return nil
}

// EvalSymlinks resolves a pin for identity comparisons only. Mutations never
// use the resulting pathname: they always use the original descriptor.
func EvalSymlinks(path string) (string, error) {
	_, _, pinned, err := relative(path)
	if err != nil {
		return "", err
	}
	if !pinned {
		return filepath.EvalSymlinks(path)
	}
	file, err := Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var buffer [4096]byte
	// FcntlFlock is x/sys's pointer-argument fcntl wrapper. F_GETPATH writes
	// MAXPATHLEN bytes instead of a flock; keep the entire backing buffer alive.
	err = unix.FcntlFlock(file.Fd(), unix.F_GETPATH, (*unix.Flock_t)(unsafe.Pointer(&buffer[0])))
	if err != nil {
		return "", err
	}
	return unix.ByteSliceToString(buffer[:]), nil
}
