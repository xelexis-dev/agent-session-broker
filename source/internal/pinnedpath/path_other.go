//go:build !darwin

// Package pinnedpath supplies filesystem operations for directory capability
// paths. On macOS /dev/fd/N paths are interpreted with descriptor-relative
// syscalls; other platforms retain their native os path behavior.
package pinnedpath

import (
	"os"
	"path/filepath"
)

var (
	Open         = os.Open
	OpenFile     = os.OpenFile
	Stat         = os.Stat
	Lstat        = os.Lstat
	ReadDir      = os.ReadDir
	CreateTemp   = os.CreateTemp
	Link         = os.Link
	Rename       = os.Rename
	Remove       = os.Remove
	Mkdir        = os.Mkdir
	EvalSymlinks = filepath.EvalSymlinks
)
