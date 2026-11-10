package serverruntime

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupCodeStartupLogContainsOnlyLocation(t *testing.T) {
	dir := t.TempDir()
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	_, closeStore, err := newManagedHandler(dir, nil)
	if err != nil {
		t.Fatal("managed setup startup failed")
	}
	defer closeStore()
	path := filepath.Join(dir, "control", "setup-code")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("proof file missing")
	}
	if !strings.Contains(output.String(), path) || strings.Contains(output.String(), strings.TrimSpace(string(raw))) {
		t.Fatal("startup log proof/location invariant failed")
	}
}
