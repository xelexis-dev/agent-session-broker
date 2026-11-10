//go:build windows

package pathguard

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func NewPin(canonicalPath, operationPath string, identity os.FileInfo, labels PinLabels) (Pin, error) {
	name, err := windows.UTF16PtrFromString(operationPath)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open pinned %s directory: %w", labels.Store, err)
	}
	file := os.NewFile(uintptr(handle), operationPath)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("open pinned %s directory: invalid handle", labels.Store)
	}
	pin := &directoryPin{file: file, operationPath: operationPath, canonicalPath: canonicalPath, identity: identity, labels: labels}
	if err := pin.Validate(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return pin, nil
}
