package groups

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound          = errors.New("session membership not found")
	ErrInvalidInput      = errors.New("invalid session membership input")
	ErrAlreadyRegistered = errors.New("session membership already registered")
	ErrOwnershipConflict = errors.New("session ownership conflict")
	ErrSelectorConflict  = errors.New("session selector already registered in project")
	ErrNameConflict      = ErrSelectorConflict
	ErrCorrupt           = errors.New("session membership registry is corrupt")
	ErrStorageBusy       = errors.New("session membership storage is busy")
	ErrClosed            = errors.New("session membership registry is closed")
)

// ProjectDirectory is a pinned capability for one encoded project directory.
type ProjectDirectory interface {
	CanonicalPath() string
	OperationPath() string
	Validate() error
}

// ProjectStorage is a Registry-owned capability for one encoded project
// directory. CanonicalPath is suitable for identity and containment checks;
// OperationPath remains anchored to the Registry's pinned directory handles.
// Callers must validate the capability before and after constructing storage.
type ProjectStorage struct {
	canonicalPath string
	operationPath string
	validate      func() error
}

func (s ProjectStorage) CanonicalPath() string { return s.canonicalPath }

func (s ProjectStorage) OperationPath() string { return s.operationPath }

func (s ProjectStorage) Validate() error {
	if s.canonicalPath == "" || s.operationPath == "" || s.validate == nil {
		return fmt.Errorf("%w", ErrCorrupt)
	}
	return s.validate()
}
