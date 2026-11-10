//go:build !darwin

package channel

import (
	"context"
	"github.com/xelexis-dev/agent-session-broker/source/internal/nativeinbox"
)

// Cross-ID native inbox semantics have only been verified on this macOS host.
func claudeParentProcessKey(context.Context, int) (string, error) {
	return "", nativeinbox.ErrUnavailable
}
