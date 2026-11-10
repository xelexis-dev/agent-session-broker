package clientstate

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestReplaceBlockedWindowsErrors(t *testing.T) {
	for _, code := range []error{windows.ERROR_ACCESS_DENIED, windows.ERROR_SHARING_VIOLATION} {
		if !replaceBlocked(fmt.Errorf("wrapped: %w", code)) {
			t.Fatal("transient Windows error rejected")
		}
	}
	for _, err := range []error{nil, windows.ERROR_FILE_NOT_FOUND, errors.New("final")} {
		if replaceBlocked(err) {
			t.Fatal("final error classified as retryable")
		}
	}
}

func TestReplaceSaveSessionReaderReleased(t *testing.T) { testReplaceOpenReader(t, true) }
func TestReplaceSaveSessionReaderHeld(t *testing.T)     { testReplaceOpenReader(t, false) }

func testReplaceOpenReader(t *testing.T, release bool) {
	t.Helper()
	f := newReplaceTarget(t, "session")
	reader, err := os.Open(f.path)
	if err != nil {
		t.Fatal("open reader")
	}
	t.Cleanup(func() { _ = reader.Close() })
	oldRename := renameFile
	t.Cleanup(func() { renameFile = oldRename })
	first := make(chan error, 1)
	var once sync.Once
	renameFile = func(from, to string) error {
		err := os.Rename(from, to)
		once.Do(func() { first <- err })
		return err
	}
	done := make(chan error, 1)
	finished := make(chan struct{})
	start := time.Now()
	go func() { defer close(finished); done <- f.write() }()
	t.Cleanup(func() {
		_ = reader.Close()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("replacement writer did not stop")
		}
	})
	select {
	case err := <-first:
		if err != nil && !replaceBlocked(err) {
			t.Fatal("reader fixture produced a final error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("replacement did not attempt rename")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("replacement failed before reader release or deadline")
		}
		f.check()
		noReplaceTemp(t, f.path)
		t.Skip("Go permits replacing this open reader within 100 ms")
	case <-time.After(100 * time.Millisecond):
	}
	if release {
		if reader.Close() != nil {
			t.Fatal("close reader")
		}
	}
	select {
	case err := <-done:
		if release {
			if err != nil {
				t.Fatal("replacement failed after reader close")
			}
			f.check()
			noReplaceTemp(t, f.path)
		} else {
			if err == nil || time.Since(start) < 2*time.Second {
				t.Fatal("held reader did not exhaust the replacement budget")
			}
			oldReplaceBytes(t, f)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("replacement exceeded watchdog")
	}
}
