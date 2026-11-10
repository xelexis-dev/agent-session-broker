package pathguard

import (
	"fmt"
	"os"
)

// PinLabels preserves each store's error vocabulary and invalid-input identity.
type PinLabels struct {
	Store, Access string
	Invalid       error
}

type directoryPin struct {
	file                         *os.File
	operationPath, canonicalPath string
	identity                     os.FileInfo
	labels                       PinLabels
}

func (p *directoryPin) Path() string { return p.operationPath }

func (p *directoryPin) Validate() error {
	info, err := p.file.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() || !os.SameFile(p.identity, info) {
		return fmt.Errorf("pinned %s directory identity changed", p.labels.Store)
	}
	canonicalInfo, _, err := InspectDirectory(p.canonicalPath)
	if err != nil {
		return err
	}
	if !os.SameFile(p.identity, canonicalInfo) {
		return fmt.Errorf("pinned %s directory no longer matches canonical path", p.labels.Store)
	}
	return nil
}

func (p *directoryPin) Close() error { return p.file.Close() }

func unsupportedPin(platform string, labels PinLabels) (Pin, error) {
	return nil, fmt.Errorf("%w: %s storage is unsupported on %s; supported platforms are Windows and Linux", labels.Invalid, labels.Store, platform)
}
