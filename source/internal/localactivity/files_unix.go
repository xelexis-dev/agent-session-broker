//go:build !windows

package localactivity

import (
	"golang.org/x/sys/unix"
	"os"
)

func readFlags() int                  { return os.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK }
func aliasFile(info os.FileInfo) bool { return info.Mode()&os.ModeSymlink != 0 }
