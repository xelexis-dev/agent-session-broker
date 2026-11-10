package controlpanel

import (
	"crypto/rand"
	"errors"
	"os"
)

// The control ownership lock is held throughout preparation, including stale-temp recovery.
func (s *Store) prepareSetupCode() error {
	if s.data.Admin != nil {
		_ = s.root.Remove("setup-code")
		_ = s.root.Remove("setup-code.tmp")
		return nil
	}
	if info, err := s.root.Lstat("setup-code.tmp"); err == nil {
		if !info.Mode().IsRegular() {
			return ErrStorage
		}
		if s.root.Remove("setup-code.tmp") != nil {
			return ErrStorage
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrStorage
	}
	code := rand.Text()
	file, err := s.root.OpenFile("setup-code.tmp", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return ErrStorage
	}
	defer s.root.Remove("setup-code.tmp")
	_, writeErr := file.WriteString(code + "\n")
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return ErrStorage
	}
	if s.root.Rename("setup-code.tmp", "setup-code") != nil {
		return ErrStorage
	}
	s.setupCode = code
	return nil
}
