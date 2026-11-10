//go:build windows

package attachmentclient

import "golang.org/x/sys/windows"

func fullDiskError() error { return windows.ERROR_DISK_FULL }
