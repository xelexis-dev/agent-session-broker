//go:build windows

package localactivity

import (
	"os"
	"syscall"
)

func readFlags() int { return os.O_RDONLY }
func aliasFile(info os.FileInfo) bool {
	attrs, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return !ok || attrs.FileAttributes&0x400 != 0
}
