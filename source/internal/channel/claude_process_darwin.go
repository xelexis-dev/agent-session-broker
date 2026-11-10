//go:build darwin

package channel

import (
	"context"
	"github.com/xelexis-dev/agent-session-broker/source/internal/processinfo"
)

func claudeParentProcessKey(ctx context.Context, pid int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	info, err := processinfo.Read(pid)
	return info.Key, err
}
