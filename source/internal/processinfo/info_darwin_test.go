package processinfo

import (
	"os"
	"testing"
)

func TestCurrentProcessIdentity(t *testing.T) {
	info, err := Read(os.Getpid())
	if err != nil || info.Key == "" || info.Executable == "" || info.Parent != os.Getppid() {
		t.Fatalf("Read=%+v, %v", info, err)
	}
	again, err := Read(os.Getpid())
	if err != nil || again.Key != info.Key {
		t.Fatalf("unstable key: %+v, %v", again, err)
	}
	parent, err := Parent(os.Getpid())
	if err != nil || parent != os.Getppid() {
		t.Fatalf("parent=%d, %v", parent, err)
	}
	if _, err := Read(-1); err == nil {
		t.Fatal("accepted invalid PID")
	}
}
