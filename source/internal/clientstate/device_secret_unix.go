//go:build darwin || linux

package clientstate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Anchor every component with openat(O_NOFOLLOW), including first creation.
// Publication is a no-replace hard link of a completely written private file.
func loadDeviceSecretPlatform(ctx context.Context, path string, create bool) ([]byte, string, error) {
	if !filepath.IsAbs(path) || filepath.Base(path) != "device-secret" {
		return nil, "path-unavailable", errDeviceIdentity
	}
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(filepath.Dir(path)), "/"), "/")
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "path-unavailable", errDeviceIdentity
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			unix.Close(fd)
			return nil, "path-unavailable", errDeviceIdentity
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if create && errors.Is(e, unix.ENOENT) {
			if e = unix.Mkdirat(fd, part, 0700); e == nil || errors.Is(e, unix.EEXIST) {
				next, e = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			} else if errors.Is(e, unix.EACCES) || errors.Is(e, unix.EROFS) || errors.Is(e, unix.EPERM) {
				unix.Close(fd)
				return nil, "directory-unwritable", errDeviceIdentity
			}
		}
		unix.Close(fd)
		if e != nil {
			if errors.Is(e, unix.ELOOP) || errors.Is(e, unix.ENOTDIR) {
				return nil, "directory-insecure", errDeviceIdentity
			}
			return nil, "path-unavailable", errDeviceIdentity
		}
		fd = next
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Uid != uint32(os.Getuid()) || st.Mode&0777 != 0700 {
		return nil, "directory-insecure", errDeviceIdentity
	}
	openPrivate := func(name string, flags int) (*os.File, error) {
		n, e := unix.Openat(fd, name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if e != nil {
			return nil, e
		}
		var info unix.Stat_t
		if unix.Fstat(n, &info) != nil || info.Mode&unix.S_IFMT != unix.S_IFREG || info.Mode&0777 != 0600 || info.Uid != uint32(os.Getuid()) || info.Nlink != 1 {
			unix.Close(n)
			return nil, errDeviceIdentity
		}
		return os.NewFile(uintptr(n), name), nil
	}
	var lockInfo unix.Stat_t
	lockPresent := unix.Fstatat(fd, "device-secret.lock", &lockInfo, unix.AT_SYMLINK_NOFOLLOW) == nil
	lockFlags := unix.O_RDWR
	if create {
		lockFlags |= unix.O_CREAT
	}
	var lock *os.File
	// Darwin can return ENOENT to concurrent O_CREAT calls even in a pinned
	// directory. Retry only that bounded creation race, never validation errors.
	for attempt := 0; attempt < 8; attempt++ {
		lock, err = openPrivate("device-secret.lock", lockFlags)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.ENOENT) {
			return nil, "lock-failed", errDeviceIdentity
		}
		select {
		case <-ctx.Done():
			return nil, "", errDeviceIdentity
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err != nil {
		return nil, "lock-failed", errDeviceIdentity
	}
	defer lock.Close()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return nil, "lock-failed", errDeviceIdentity
		}
		select {
		case <-ctx.Done():
			return nil, "", errDeviceIdentity
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	read := func() ([]byte, string, error) {
		f, e := openPrivate("device-secret", unix.O_RDONLY)
		if e != nil {
			return nil, "secret-insecure", e
		}
		defer f.Close()
		raw, e := io.ReadAll(io.LimitReader(f, 66))
		if e != nil {
			return nil, "secret-insecure", errDeviceIdentity
		}
		if len(raw) != 65 || raw[64] != '\n' {
			return nil, "secret-malformed", errDeviceIdentity
		}
		key, e := hex.DecodeString(string(raw[:64]))
		if e != nil || len(key) != 32 {
			return nil, "secret-malformed", errDeviceIdentity
		}
		return key, "ok", nil
	}
	key, cause, err := read()
	if err == nil {
		return key, cause, nil
	}
	if !create || !errors.Is(err, unix.ENOENT) {
		if errors.Is(err, unix.ENOENT) {
			cause = "secret-missing"
		}
		return nil, cause, errDeviceIdentity
	}
	key = make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return nil, "secret-create-failed", errDeviceIdentity
	}
	tmp := ".device-" + rand.Text()
	f, err := openPrivate(tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL)
	if err != nil {
		return nil, "secret-create-failed", errDeviceIdentity
	}
	defer unix.Unlinkat(fd, tmp, 0)
	_, err = f.Write([]byte(hex.EncodeToString(key) + "\n"))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return nil, "secret-create-failed", errDeviceIdentity
	}
	if err = unix.Linkat(fd, tmp, fd, "device-secret", 0); err != nil {
		return nil, "secret-create-failed", errDeviceIdentity
	}
	if unix.Unlinkat(fd, tmp, 0) != nil || unix.Fsync(fd) != nil {
		return nil, "secret-create-failed", errDeviceIdentity
	}
	key, cause, err = read()
	if err == nil {
		cause = "secret-created"
		if lockPresent {
			cause = "secret-created-lock-present"
		}
	}
	return key, cause, err
}
