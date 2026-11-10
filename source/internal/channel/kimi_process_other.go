//go:build !darwin && !linux

package channel

import "os"

func kimiProcessKey(int) (string, error) { return "", os.ErrNotExist }
func kimiParentPID(int) (int, error)     { return 0, os.ErrNotExist }
