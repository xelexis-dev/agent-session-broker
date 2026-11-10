//go:build darwin

package codex

import (
	"context"
	"github.com/xelexis-dev/agent-session-broker/source/internal/processinfo"
	"path/filepath"
)

func externalProcessKey(ctx context.Context, pid int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	info, err := processinfo.Read(pid)
	if err != nil {
		return "", err
	}
	if filepath.Base(info.Executable) != "codex" {
		return "", ErrSessionNotFound
	}
	return info.Key, ctx.Err()
}
