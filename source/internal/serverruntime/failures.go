package serverruntime

import (
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// RunError contains only closed classifications, never the original error or input.
type RunError struct{ Reason, Detail, Flag string }

func (e *RunError) Error() string                            { return e.Reason }
func (e *RunError) Classification() (string, string, string) { return e.Reason, e.Detail, e.Flag }
func storageFailure(err error) *RunError {
	switch {
	case errors.Is(err, controlpanel.ErrDataDirLocked):
		return &RunError{Reason: "data_dir_locked"}
	case errors.Is(err, os.ErrPermission):
		return &RunError{Reason: "data_dir_permission"}
	default:
		return &RunError{Reason: "storage_failed"}
	}
}
func listenFailure(err error) *RunError {
	detail := "invalid_address"
	switch {
	case errors.Is(err, syscall.EADDRINUSE) || (runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(10048))):
		detail = "addr_in_use"
	case errors.Is(err, os.ErrPermission) || (runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(10013))):
		detail = "permission"
	}
	return &RunError{Reason: "listen_failed", Detail: detail}
}

// ErrorEvent uses type identity, never Error() text, even for unknown failures.
func ErrorEvent(err error) serverdiag.Event {
	e := serverdiag.Event{Type: "server-failed", Reason: "storage_failed"}
	var classified *RunError
	if errors.As(err, &classified) {
		e.Reason = classified.Reason
		e.Detail = classified.Detail
		e.Flag = classified.Flag
	}
	return e
}
func invalidFlag(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" || !strings.HasPrefix(arg, "-") {
			return ""
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		switch name {
		case "version":
			if hasValue {
				if _, err := strconv.ParseBool(value); err != nil {
					return name
				}
			}
		case "listen", "memory-dir", "trusted-proxy":
			if !hasValue {
				i++
				if i >= len(args) {
					return name
				}
				value = args[i]
			}
			if name == "trusted-proxy" {
				if _, err := netip.ParsePrefix(value); err != nil {
					return name
				}
			}
		default:
			return ""
		}
	}
	return ""
}
