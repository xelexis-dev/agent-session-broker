//go:build windows

package channel

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsParentEnumerationErrors(t *testing.T) {
	for _, tc := range []struct {
		name, step        string
		firstErr, nextErr error
		self              bool
	}{
		{name: "first", step: "first", firstErr: windows.ERROR_ACCESS_DENIED},
		{name: "partial-next", step: "next", nextErr: windows.ERROR_BAD_LENGTH, self: true},
		{name: "empty", step: "selfMissing", firstErr: windows.ERROR_NO_MORE_FILES},
		{name: "self-missing", step: "selfMissing"},
		{name: "normal-eof", self: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pids, err := windowsAncestorPIDs(42, func(e *windows.ProcessEntry32) error {
				if tc.self {
					e.ProcessID = 42
					e.ParentProcessID = 10
				}
				return tc.firstErr
			}, func(*windows.ProcessEntry32) error {
				if tc.nextErr != nil {
					return tc.nextErr
				}
				return windows.ERROR_NO_MORE_FILES
			})
			if tc.step == "" {
				if err != nil || len(pids) != 1 || pids[0] != 10 {
					t.Fatal("normal enumeration rejected")
				}
				return
			}
			var failure *parentObservationError
			if !errors.As(err, &failure) || failure.step != tc.step || len(pids) != 0 {
				t.Fatal("enumeration failure lost or partial ancestry accepted")
			}
			cause := tc.firstErr
			if tc.nextErr != nil {
				cause = tc.nextErr
			}
			if tc.step != "selfMissing" && !errors.Is(err, cause) {
				t.Fatal("underlying enumeration error discarded")
			}
		})
	}
}

func TestWindowsParentEnumerationCycle(t *testing.T) {
	_, err := windowsAncestorPIDs(42, func(e *windows.ProcessEntry32) error { e.ProcessID = 42; e.ParentProcessID = 42; return nil }, func(*windows.ProcessEntry32) error { return windows.ERROR_NO_MORE_FILES })
	var failure *parentObservationError
	if !errors.As(err, &failure) || failure.step != "cycle" {
		t.Fatal("cycle accepted")
	}
}
