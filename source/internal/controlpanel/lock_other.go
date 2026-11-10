//go:build !windows && !linux && !darwin

package controlpanel

import "os"

func lockFile(*os.File) error { return ErrStorage }
