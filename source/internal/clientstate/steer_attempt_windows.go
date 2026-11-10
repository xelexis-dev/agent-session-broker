//go:build windows

package clientstate

import (
	"golang.org/x/sys/windows"
	"os"
)

func protectSteerPath(path string) error {
	sd, err := privateDeviceSD()
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, acl, nil)
}
func privateSteerPath(path string, _ os.FileInfo, strict bool) bool {
	want, err := privateDeviceSD()
	if err != nil {
		return false
	}
	got, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	if strict {
		return devicePrivateDACL(got, want)
	}
	a, _, e := got.Owner()
	b, _, e2 := want.Owner()
	return e == nil && e2 == nil && a.Equals(b)
}

func lockExistingSteerFile(path string) (*os.File, func(), error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, nil, ErrSteerRecovery
	}
	overlapped := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped); err != nil {
		file.Close()
		return nil, nil, ErrLocked
	}
	return file, func() { _ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped); _ = file.Close() }, nil
}
