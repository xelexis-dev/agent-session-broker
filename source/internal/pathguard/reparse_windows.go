//go:build windows

package pathguard

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const fileAttributeReparsePoint = 0x400

func hasReparsePoint(info os.FileInfo) bool {
	attributes, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return true
	}
	return attributes.FileAttributes&fileAttributeReparsePoint != 0
}

func permitsShortPathComponent(component, resolved string, identity os.FileInfo) bool {
	shortPath, err := getShortPathName(resolved)
	if err != nil || !strings.EqualFold(component, filepath.Base(shortPath)) {
		return false
	}
	resolvedInfo, err := os.Lstat(resolved)
	return err == nil && resolvedInfo.IsDir() && resolvedInfo.Mode()&os.ModeSymlink == 0 && !hasReparsePoint(resolvedInfo) && os.SameFile(identity, resolvedInfo)
}

func matchesCanonicalComponent(component, resolved string) bool {
	return strings.EqualFold(component, filepath.Base(resolved))
}

func hasSafePathSpelling(path string) bool {
	for _, component := range strings.FieldsFunc(path, func(character rune) bool {
		return character <= 0xff && os.IsPathSeparator(uint8(character))
	}) {
		if component != "." && component != ".." && (strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ")) {
			return false
		}
	}
	return true
}

func getShortPathName(path string) (string, error) {
	pathPointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	buffer := make([]uint16, syscall.MAX_PATH)
	length, err := syscall.GetShortPathName(pathPointer, &buffer[0], uint32(len(buffer)))
	if err != nil {
		return "", err
	}
	if length >= uint32(len(buffer)) {
		buffer = make([]uint16, length+1)
		length, err = syscall.GetShortPathName(pathPointer, &buffer[0], uint32(len(buffer)))
		if err != nil {
			return "", err
		}
	}
	return syscall.UTF16ToString(buffer[:length]), nil
}
