//go:build darwin || linux || windows

package localactivity

import (
	"context"
	"os"
	"runtime"
	"testing"
)

func TestNativeProcessCreationIdentity(t *testing.T) {
	first, err := nativeProcess(context.Background(), os.Getpid())
	if err != nil || first.Key == "" || first.Host == "" || first.Platform != runtime.GOOS {
		t.Fatal("native creation identity unavailable")
	}
	second, err := nativeProcess(context.Background(), os.Getpid())
	if err != nil || second != first {
		t.Fatal("creation identity changed for same live process")
	}
	if _, err := nativeProcess(context.Background(), -1); err == nil {
		t.Fatal("invalid PID accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := nativeProcess(ctx, os.Getpid()); err == nil {
		t.Fatal("cancelled probe accepted")
	}
}
