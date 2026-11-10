package channel

import (
	"os"
	"testing"
)

func TestDarwinParentDiscovery(t *testing.T) {
	parents, err := ancestorPIDs()
	if err != nil || len(parents) == 0 || parents[0] != os.Getppid() {
		t.Fatalf("parents=%v err=%v", parents, err)
	}
	harness, err := processHarness(os.Getpid())
	if err != nil || harness != "" {
		t.Fatalf("test process classified as agent: %q %v", harness, err)
	}
}
