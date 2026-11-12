//go:build windows

package accesslog

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"syscall"
	"unsafe"
)

func relativeHandle(dir *os.File, name string, access, disposition uint32) (windows.Handle, error) {
	object, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	attrs := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(dir.Fd()), ObjectName: object, Attributes: windows.OBJ_CASE_INSENSITIVE}
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var handle windows.Handle
	var io windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&handle, access|windows.SYNCHRONIZE, &attrs, &io, nil, windows.FILE_ATTRIBUTE_NORMAL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, disposition, windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	return handle, windowsError(err)
}
func openLogFile(root *os.Root, dir *os.File, name string, create bool) (*os.File, error) {
	disposition := uint32(windows.FILE_OPEN)
	if create {
		disposition = windows.FILE_CREATE
	}
	handle, err := relativeHandle(dir, name, windows.FILE_GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES, disposition)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), name)
	if _, err = file.Seek(0, 2); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
func regular(info os.FileInfo) bool {
	if !info.Mode().IsRegular() {
		return false
	}
	attrs, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && attrs.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0
}

type renameInfo struct {
	Replace uint32
	Root    windows.Handle
	Length  uint32
	Name    [1]uint16
}

func renameExclusive(dir *os.File, from, to string) error {
	handle, err := relativeHandle(dir, from, windows.DELETE|windows.FILE_READ_ATTRIBUTES, windows.FILE_OPEN)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	name, err := windows.UTF16FromString(to)
	if err != nil {
		return err
	}
	name = name[:len(name)-1]
	var layout renameInfo
	offset := unsafe.Offsetof(layout.Name)
	buffer := make([]byte, int(offset)+len(name)*2)
	info := (*renameInfo)(unsafe.Pointer(&buffer[0]))
	info.Root = windows.Handle(dir.Fd())
	info.Length = uint32(len(name) * 2)
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&buffer[offset])), len(name)), name)
	var io windows.IO_STATUS_BLOCK
	// Replace remains zero: a concurrent destination is never overwritten.
	return windowsError(windows.NtSetInformationFile(handle, &io, &buffer[0], uint32(len(buffer)), windows.FileRenameInformation))
}

func windowsError(err error) error {
	var status windows.NTStatus
	if errors.As(err, &status) {
		return status.Errno()
	}
	return err
}
