//go:build windows

package attachmentclient

import (
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"strings"
)

func finalPath(file *os.File) (string, error) {
	b := make([]uint16, 32768)
	n, err := windows.GetFinalPathNameByHandle(windows.Handle(file.Fd()), &b[0], uint32(len(b)), 0)
	if err != nil || n >= uint32(len(b)) {
		return "", ErrUnsupported
	}
	s := windows.UTF16ToString(b[:n])
	if strings.HasPrefix(s, `\\?\UNC\`) {
		s = `\\` + strings.TrimPrefix(s, `\\?\UNC\`)
	} else {
		s = strings.TrimPrefix(s, `\\?\`)
	}
	return s, nil
}
func canonicalCWD(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return rootLocation(f)
}
func rootLocation(f *os.File) (string, error) {
	s, err := finalPath(f)
	if err != nil || strings.HasPrefix(s, `\\`) {
		return "", ErrUnsupported
	}
	root, _ := windows.UTF16PtrFromString(filepath.VolumeName(s) + `\`)
	if windows.GetDriveType(root) == windows.DRIVE_REMOTE {
		return "", ErrUnsupported
	}
	return s, nil
}

func openLeaf(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}
func validateLeaf(f *os.File, info os.FileInfo, cwd, state string) error {
	var data windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &data) != nil || data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || data.NumberOfLinks != 1 {
		return failure("attachment-link")
	}
	if !info.Mode().IsRegular() {
		return failure("attachment-not-file")
	}
	path, err := finalPath(f)
	if err != nil {
		return ErrUnsupported
	}
	rel, err := filepath.Rel(cwd, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, `..\`) || filepath.IsAbs(rel) {
		return failure("attachment-outside-project")
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		if invalidSpelling(part) {
			return failure("attachment-outside-project")
		}
	}
	if denied(parts) || inTree(state, path) {
		return failure("attachment-credential")
	}
	return nil
}
