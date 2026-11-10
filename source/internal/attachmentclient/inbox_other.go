//go:build !darwin && (!linux || android) && !windows

package attachmentclient

import "os"

func renameAbsent(string, string) error   { return ErrUnsupported }
func createPart(string) (*os.File, error) { return nil, ErrUnsupported }
func createLock(string) (*os.File, error) { return nil, ErrUnsupported }
func lockFile(*os.File) (func(), error)   { return nil, ErrUnsupported }
func diskFull(error) bool                 { return false }
