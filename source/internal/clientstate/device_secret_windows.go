//go:build windows

package clientstate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"
)

func privateDeviceSD() (*windows.SECURITY_DESCRIPTOR, error) {
	u, e := windows.GetCurrentProcessToken().GetTokenUser()
	if e != nil {
		return nil, e
	}
	sid := u.User.Sid.String()
	return windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;;FA;;;" + sid + ")")
}
func devicePrivateDACL(sd, want *windows.SECURITY_DESCRIPTOR) bool {
	a, _, e := sd.Owner()
	b, _, e2 := want.Owner()
	if e != nil || e2 != nil || a == nil || b == nil || !a.Equals(b) {
		return false
	}
	acl := func(s string) string {
		i := strings.Index(s, "D:")
		if i < 0 {
			return ""
		}
		s = s[i:]
		if j := strings.Index(s, "S:"); j >= 0 {
			s = s[:j]
		}
		return s
	}
	return acl(sd.String()) != "" && acl(sd.String()) == acl(want.String())
}
func loadDeviceSecretPlatform(ctx context.Context, path string, create bool) ([]byte, string, error) {
	if !filepath.IsAbs(path) || filepath.Base(path) != "device-secret" {
		return nil, "path-unavailable", errDeviceIdentity
	}
	sd, err := privateDeviceSD()
	if err != nil {
		return nil, "directory-insecure", errDeviceIdentity
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	// Pin ancestors without FILE_SHARE_DELETE so reparse/rename replacement
	// cannot redirect subsequent file operations to a different directory.
	var pins []windows.Handle
	defer func() {
		for _, h := range pins {
			windows.CloseHandle(h)
		}
	}()
	dir := filepath.Dir(path)
	volume := filepath.VolumeName(dir)
	current := volume + string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(dir, current), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		p, e := windows.UTF16PtrFromString(current)
		if e != nil {
			return nil, "directory-insecure", errDeviceIdentity
		}
		if create {
			e = windows.CreateDirectory(p, sa)
			if e != nil && !errors.Is(e, windows.ERROR_ALREADY_EXISTS) {
				return nil, "directory-unwritable", errDeviceIdentity
			}
		}
		h, e := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if e != nil {
			return nil, "directory-insecure", errDeviceIdentity
		}
		pins = append(pins, h)
		var info windows.ByHandleFileInformation
		if windows.GetFileInformationByHandle(h, &info) != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return nil, "directory-insecure", errDeviceIdentity
		}
		if current == dir {
			actual, e := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
			if e != nil || !devicePrivateDACL(actual, sd) {
				return nil, "directory-insecure", errDeviceIdentity
			}
		}
	}
	open := func(name string, access, disposition uint32) (*os.File, error) {
		p, e := windows.UTF16PtrFromString(name)
		if e != nil {
			return nil, e
		}
		h, e := windows.CreateFile(p, access|windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, sa, disposition, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if e != nil {
			return nil, e
		}
		fail := func() (*os.File, error) { windows.CloseHandle(h); return nil, errDeviceIdentity }
		var info windows.ByHandleFileInformation
		if windows.GetFileInformationByHandle(h, &info) != nil || info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks != 1 {
			return fail()
		}
		actual, e := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if e != nil || !devicePrivateDACL(actual, sd) {
			return fail()
		}
		return os.NewFile(uintptr(h), name), nil
	}
	_, lockStatErr := os.Lstat(path + ".lock")
	lockPresent := lockStatErr == nil
	disposition := uint32(windows.OPEN_EXISTING)
	if create {
		disposition = windows.OPEN_ALWAYS
	}
	lock, err := open(path+".lock", windows.GENERIC_READ|windows.GENERIC_WRITE, disposition)
	if err != nil {
		return nil, "lock-failed", errDeviceIdentity
	}
	defer lock.Close()
	ov := new(windows.Overlapped)
	for {
		err = windows.LockFileEx(windows.Handle(lock.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ov)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, "lock-failed", errDeviceIdentity
		}
		select {
		case <-ctx.Done():
			return nil, "", errDeviceIdentity
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer windows.UnlockFileEx(windows.Handle(lock.Fd()), 0, 1, 0, ov)
	read := func() ([]byte, string, error) {
		f, e := open(path, windows.GENERIC_READ, windows.OPEN_EXISTING)
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
	if !create || (!errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, os.ErrNotExist)) {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, os.ErrNotExist) {
			cause = "secret-missing"
		}
		return nil, cause, errDeviceIdentity
	}
	key = make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return nil, "secret-create-failed", errDeviceIdentity
	}
	tmp := filepath.Join(dir, ".device-"+rand.Text())
	f, err := open(tmp, windows.GENERIC_WRITE, windows.CREATE_NEW)
	if err != nil {
		return nil, "secret-create-failed", errDeviceIdentity
	}
	defer os.Remove(tmp)
	_, err = f.Write([]byte(hex.EncodeToString(key) + "\n"))
	if err == nil {
		err = f.Sync()
	}
	closed := f.Close()
	if err != nil || closed != nil {
		return nil, "secret-create-failed", errDeviceIdentity
	}
	if os.Link(tmp, path) != nil || os.Remove(tmp) != nil {
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
