//go:build !darwin && !linux && !windows

package accesslog

import "os"

func openLogFile(*os.Root, *os.File, string, bool) (*os.File, error) { return nil, os.ErrInvalid }
func renameExclusive(*os.File, string, string) error                 { return os.ErrInvalid }
func regular(info os.FileInfo) bool                                  { return info.Mode().IsRegular() }
