//go:build !darwin && (!linux || android) && !windows

package attachmentclient

import "os"

func canonicalCWD(string) (string, error)                      { return "", ErrUnsupported }
func openLeaf(string) (*os.File, error)                        { return nil, ErrUnsupported }
func validateLeaf(*os.File, os.FileInfo, string, string) error { return ErrUnsupported }

func rootLocation(*os.File) (string, error) { return "", ErrUnsupported }
