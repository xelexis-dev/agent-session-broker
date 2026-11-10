// Package pathguard creates private directory trees without traversing aliases.
// It validates every existing ancestor before the first filesystem mutation and
// anchors each subsequent single-component create to a pinned parent directory.
package pathguard

import (
	"errors"
	"fmt"
	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
	"os"
	"path/filepath"
	"strings"
)

// Directory is a validated, handle-anchored directory capability.
type Directory interface {
	Path() string
	Validate() error
}

// Pin is an owned Directory capability.
type Pin interface {
	Directory
	Close() error
}

// OpenPin opens and validates a directory handle. canonicalPath is the stable
// path used for full-chain validation; operationPath may be handle-relative.
type OpenPin func(canonicalPath, operationPath string, identity os.FileInfo) (Pin, error)

// CreateHook runs after the pinned parent has been checked and immediately
// before it is checked again for a single-component create. It exists so
// platform race tests can deterministically exercise this boundary.
type CreateHook func() error

// EnsureDirectory validates an absolute directory chain and creates missing
// components one at a time through successively pinned parents.
func EnsureDirectory(requested string, perm os.FileMode, open OpenPin, hook CreateHook) (string, os.FileInfo, Pin, error) {
	if open == nil {
		return "", nil, nil, errors.New("directory pin opener is unavailable")
	}
	if !hasSafePathSpelling(requested) {
		return "", nil, nil, errors.New("directory path uses an unsafe spelling")
	}
	absolute, err := filepath.Abs(requested)
	if err != nil {
		return "", nil, nil, fmt.Errorf("resolve directory: %w", err)
	}
	absolute = filepath.Clean(absolute)
	walk, err := walkToNearestExisting(absolute)
	if err != nil {
		return "", nil, nil, err
	}

	inner, err := open(walk.canonical, walk.operation, walk.identity)
	if err != nil {
		return "", nil, nil, fmt.Errorf("pin nearest existing directory: %w", err)
	}
	current := guardPin(inner, walk.canonical, walk.identity)
	if err := current.Validate(); err != nil {
		_ = current.Close()
		return "", nil, nil, fmt.Errorf("revalidate nearest existing directory: %w", err)
	}
	currentCanonical := walk.canonical
	currentIdentity := walk.identity

	for _, component := range walk.missing {
		childCanonical, childIdentity, child, exists, childErr := EnsureChild(current, currentCanonical, component, true, perm, open, hook)
		if childErr != nil {
			_ = current.Close()
			return "", nil, nil, childErr
		}
		if !exists || child == nil {
			_ = current.Close()
			return "", nil, nil, errors.New("created directory is unavailable")
		}
		if err := current.Close(); err != nil {
			_ = child.Close()
			return "", nil, nil, fmt.Errorf("close ancestor directory pin: %w", err)
		}
		currentCanonical = childCanonical
		currentIdentity = childIdentity
		current = child
	}

	if err := current.Validate(); err != nil {
		_ = current.Close()
		return "", nil, nil, fmt.Errorf("validate requested directory: %w", err)
	}
	return currentCanonical, currentIdentity, current, nil
}

// EnsureChild validates or creates exactly one child through parent. It never
// recursively creates paths and rejects final-component aliases and reparse
// points before returning an owned pin.
func EnsureChild(parent Directory, canonicalParent, name string, create bool, perm os.FileMode, open OpenPin, hook CreateHook) (string, os.FileInfo, Pin, bool, error) {
	if parent == nil || open == nil {
		return "", nil, nil, false, errors.New("parent directory capability is unavailable")
	}
	if err := validateLeafName(name); err != nil {
		return "", nil, nil, false, err
	}
	canonicalParent, err := filepath.Abs(canonicalParent)
	if err != nil {
		return "", nil, nil, false, fmt.Errorf("resolve parent directory: %w", err)
	}
	canonicalParent = filepath.Clean(canonicalParent)
	parentIdentity, resolvedParent, err := ValidateDirectoryPath(canonicalParent)
	if err != nil {
		return "", nil, nil, false, fmt.Errorf("validate parent directory chain: %w", err)
	}
	if err := parent.Validate(); err != nil {
		return "", nil, nil, false, fmt.Errorf("validate pinned parent directory: %w", err)
	}
	operationParentInfo, err := pfs.Stat(parent.Path())
	if err != nil || !operationParentInfo.IsDir() || !os.SameFile(parentIdentity, operationParentInfo) {
		if err == nil {
			err = errors.New("pinned parent identity does not match canonical parent")
		}
		return "", nil, nil, false, fmt.Errorf("validate parent operation directory: %w", err)
	}

	canonicalChild := filepath.Join(canonicalParent, name)
	operationChild := filepath.Join(parent.Path(), name)
	_, lstatErr := pfs.Lstat(operationChild)
	if lstatErr != nil && !errors.Is(lstatErr, os.ErrNotExist) {
		return "", nil, nil, false, fmt.Errorf("inspect child directory: %w", lstatErr)
	}
	if errors.Is(lstatErr, os.ErrNotExist) && !create {
		if err := parent.Validate(); err != nil {
			return "", nil, nil, false, fmt.Errorf("revalidate absent child parent: %w", err)
		}
		return canonicalChild, nil, nil, false, nil
	}

	created := false
	cleanup := true
	defer func() {
		if cleanup && created {
			_ = pfs.Remove(operationChild)
		}
	}()
	if errors.Is(lstatErr, os.ErrNotExist) {
		if hook != nil {
			if err := hook(); err != nil {
				return "", nil, nil, false, fmt.Errorf("before directory create: %w", err)
			}
		}
		if err := parent.Validate(); err != nil {
			return "", nil, nil, false, fmt.Errorf("revalidate parent before directory create: %w", err)
		}
		if err := pfs.Mkdir(operationChild, perm); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return "", nil, nil, false, fmt.Errorf("create child directory: %w", err)
			}
		} else {
			created = true
		}
	}

	if err := parent.Validate(); err != nil {
		return "", nil, nil, false, fmt.Errorf("revalidate parent after directory create: %w", err)
	}
	childIdentity, resolvedChild, err := ValidateDirectoryPath(canonicalChild)
	if err != nil {
		return "", nil, nil, false, fmt.Errorf("validate child directory chain: %w", err)
	}
	operationIdentity, operationResolved, err := InspectDirectory(operationChild)
	if err != nil {
		return "", nil, nil, false, fmt.Errorf("inspect child operation directory: %w", err)
	}
	if !os.SameFile(childIdentity, operationIdentity) {
		return "", nil, nil, false, errors.New("child operation directory identity mismatch")
	}
	if same, err := samePath(resolvedChild, operationResolved); err != nil || !same {
		return "", nil, nil, false, errors.New("child operation directory resolved elsewhere")
	}
	if same, err := samePath(resolvedParent, filepath.Dir(resolvedChild)); err != nil || !same {
		return "", nil, nil, false, errors.New("child directory escaped its direct parent")
	}

	inner, err := open(resolvedChild, operationChild, childIdentity)
	if err != nil {
		return "", nil, nil, false, fmt.Errorf("pin child directory: %w", err)
	}
	child := guardPin(inner, resolvedChild, childIdentity)
	if err := child.Validate(); err != nil {
		_ = child.Close()
		return "", nil, nil, false, fmt.Errorf("validate pinned child directory: %w", err)
	}
	if err := parent.Validate(); err != nil {
		_ = child.Close()
		return "", nil, nil, false, fmt.Errorf("revalidate parent after pinning child: %w", err)
	}
	cleanup = false
	return resolvedChild, childIdentity, child, true, nil
}

// InspectDirectory rejects a symlink, junction, or other reparse point at path
// and returns the directory's identity and resolved path.
func InspectDirectory(path string) (os.FileInfo, string, error) {
	linkInfo, err := pfs.Lstat(path)
	if err != nil {
		return nil, "", err
	}
	if !linkInfo.IsDir() || linkInfo.Mode()&os.ModeSymlink != 0 || hasReparsePoint(linkInfo) {
		return nil, "", errors.New("path is not a real directory")
	}
	resolved, err := pfs.EvalSymlinks(path)
	if err != nil {
		return nil, "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, "", err
	}
	resolved = filepath.Clean(resolved)
	info, err := pfs.Stat(path)
	if err != nil {
		return nil, "", err
	}
	if !info.IsDir() || !os.SameFile(linkInfo, info) {
		return nil, "", errors.New("directory identity changed while inspecting")
	}
	return info, resolved, nil
}

// ValidateDirectoryPath walks from the path's volume root through every
// component, rejecting aliases at any existing ancestor.
func ValidateDirectoryPath(path string) (os.FileInfo, string, error) {
	if !hasSafePathSpelling(path) {
		return nil, "", errors.New("directory path uses an unsafe spelling")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	walk, err := walkToNearestExisting(filepath.Clean(absolute))
	if err != nil {
		return nil, "", err
	}
	if len(walk.missing) != 0 {
		return nil, "", os.ErrNotExist
	}
	return walk.identity, walk.canonical, nil
}

type walkResult struct {
	operation string
	canonical string
	identity  os.FileInfo
	missing   []string
}

func walkToNearestExisting(absolute string) (walkResult, error) {
	return walkToNearestExistingWithHook(absolute, nil)
}

func walkToNearestExistingWithHook(absolute string, afterComponent func(string) error) (walkResult, error) {
	root, components, err := splitAbsolute(absolute)
	if err != nil {
		return walkResult{}, err
	}
	identity, resolved, err := InspectDirectory(root)
	if err != nil {
		return walkResult{}, fmt.Errorf("inspect volume root: %w", err)
	}
	operation := root
	canonical := resolved
	for index, component := range components {
		operationIdentity, operationErr := pfs.Lstat(operation)
		if operationErr != nil || !operationIdentity.IsDir() || operationIdentity.Mode()&os.ModeSymlink != 0 || hasReparsePoint(operationIdentity) || !os.SameFile(identity, operationIdentity) {
			return walkResult{}, errors.New("existing directory component changed after inspection")
		}
		candidate := filepath.Join(operation, component)
		candidateIdentity, candidateResolved, inspectErr := InspectDirectory(candidate)
		if errors.Is(inspectErr, os.ErrNotExist) {
			return walkResult{operation: operation, canonical: canonical, identity: identity, missing: append([]string(nil), components[index:]...)}, nil
		}
		if inspectErr != nil {
			return walkResult{}, fmt.Errorf("inspect existing directory component: %w", inspectErr)
		}
		expected := filepath.Join(canonical, component)
		expectedMatches, expectedErr := samePath(expected, candidateResolved)
		if expectedErr != nil {
			return walkResult{}, errors.New("existing directory component resolves through an alias")
		}
		if (!expectedMatches || !matchesCanonicalComponent(component, candidateResolved)) && !permitsShortPathComponent(component, candidateResolved, candidateIdentity) {
			return walkResult{}, errors.New("existing directory component resolves through an alias")
		}
		if same, relErr := samePath(canonical, filepath.Dir(candidateResolved)); relErr != nil || !same {
			return walkResult{}, errors.New("existing directory component escaped its direct parent")
		}
		operation = candidate
		canonical = candidateResolved
		identity = candidateIdentity
		if afterComponent != nil {
			if err := afterComponent(operation); err != nil {
				return walkResult{}, fmt.Errorf("after inspecting directory component: %w", err)
			}
		}
	}
	return walkResult{operation: operation, canonical: canonical, identity: identity}, nil
}

func splitAbsolute(absolute string) (string, []string, error) {
	if !filepath.IsAbs(absolute) {
		return "", nil, errors.New("directory path is not absolute")
	}
	volume := filepath.VolumeName(absolute)
	root := string(filepath.Separator)
	if volume != "" {
		root = volume + string(filepath.Separator)
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil {
		return "", nil, err
	}
	if relative == "." {
		return root, nil, nil
	}
	components := strings.Split(relative, string(filepath.Separator))
	for _, component := range components {
		if err := validateLeafName(component); err != nil {
			return "", nil, err
		}
	}
	return root, components, nil
}

func validateLeafName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) || !hasSafePathSpelling(name) {
		return errors.New("invalid directory component")
	}
	return nil
}

func samePath(left, right string) (bool, error) {
	relative, err := filepath.Rel(left, right)
	if err != nil {
		return false, err
	}
	return relative == ".", nil
}

type guardedPin struct {
	Pin
	canonical string
	identity  os.FileInfo
}

func guardPin(inner Pin, canonical string, identity os.FileInfo) Pin {
	return &guardedPin{Pin: inner, canonical: canonical, identity: identity}
}

func (pin *guardedPin) Validate() error {
	identity, _, err := ValidateDirectoryPath(pin.canonical)
	if err != nil {
		return err
	}
	if !os.SameFile(pin.identity, identity) {
		return errors.New("canonical directory identity changed")
	}
	return pin.Pin.Validate()
}
