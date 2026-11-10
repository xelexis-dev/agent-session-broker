//go:build !windows

package pathguard

import (
	"os"
	"path/filepath"
)

func hasReparsePoint(os.FileInfo) bool { return false }

func permitsShortPathComponent(string, string, os.FileInfo) bool { return false }

func matchesCanonicalComponent(component, resolved string) bool {
	return component == filepath.Base(resolved)
}

func hasSafePathSpelling(string) bool { return true }
