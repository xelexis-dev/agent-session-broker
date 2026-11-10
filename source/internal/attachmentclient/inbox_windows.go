//go:build windows

package attachmentclient

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func renameAbsent(old, new string) error {
	a, e := windows.UTF16PtrFromString(old)
	if e != nil {
		return e
	}
	b, e := windows.UTF16PtrFromString(new)
	if e != nil {
		return e
	}
	return windows.MoveFile(a, b)
}
func createPart(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
}
func createLock(path string) (*os.File, error) {
	name, e := windows.UTF16PtrFromString(path)
	if e != nil {
		return nil, e
	}
	h, e := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_ALWAYS, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(h), path)
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(h, &info) != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		f.Close()
		return nil, os.ErrInvalid
	}
	return f, nil
}
func lockFile(f *os.File) (func(), error) {
	ov := &windows.Overlapped{}
	if e := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ov); e != nil {
		return nil, e
	}
	return func() { _ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ov) }, nil
}
func diskFull(e error) bool {
	return errors.Is(e, windows.ERROR_DISK_FULL) || errors.Is(e, windows.ERROR_HANDLE_DISK_FULL)
}
