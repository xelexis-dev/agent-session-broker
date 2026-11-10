//go:build darwin || (linux && !android)

package attachmentclient

import "golang.org/x/sys/unix"

func fullDiskError() error { return unix.ENOSPC }
