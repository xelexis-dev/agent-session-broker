package memory

import (
	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
)

type directoryPin interface {
	Path() string
	Validate() error
	Close() error
}

type linkFileFunc func(oldPath, newPath string) error

func preflightHardLinks(directory string, link linkFileFunc) error {
	probe, err := pfs.CreateTemp(directory, ".hard-link-probe-*")
	if err != nil {
		return err
	}
	temporaryPath := probe.Name()
	linkedPath := temporaryPath + ".link"
	defer func() {
		_ = probe.Close()
		_ = pfs.Remove(linkedPath)
		_ = pfs.Remove(temporaryPath)
	}()

	if err := probe.Sync(); err != nil {
		return err
	}
	if err := probe.Close(); err != nil {
		return err
	}
	if err := link(temporaryPath, linkedPath); err != nil {
		return &hardLinkPreflightError{cause: err}
	}
	if err := pfs.Remove(linkedPath); err != nil {
		return err
	}
	if err := pfs.Remove(temporaryPath); err != nil {
		return err
	}
	return nil
}

type hardLinkPreflightError struct {
	cause error
}

func (e *hardLinkPreflightError) Error() string {
	return "hard-link commit unavailable: " + e.cause.Error()
}

func (e *hardLinkPreflightError) Unwrap() error {
	return e.cause
}
