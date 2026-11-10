//go:build !darwin && !linux && !windows

package localactivity

import "context"

// Until a native creation-identity encoding is verified, callers get unknown.
// A verified platform adapter may supply Config.Probe without changing readers.
func nativeProcess(context.Context, int) (Process, error) { return Process{}, unavailable }
