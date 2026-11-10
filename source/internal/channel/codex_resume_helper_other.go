//go:build !darwin && !linux

package channel

import "context"

func runNativeCodexResumeHelper(context.Context, daemonResumeOptions) error {
	return ErrCodexResumeIncomplete
}
