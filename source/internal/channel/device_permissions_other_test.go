//go:build !windows

package channel

import (
	"os"
	"testing"
)

func insecureSecretFixture(t *testing.T, path string) func() {
	t.Helper()
	return func() {
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal("fixture permissions")
		}
	}
}
