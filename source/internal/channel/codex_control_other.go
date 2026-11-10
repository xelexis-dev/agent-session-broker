//go:build !darwin && !linux

package channel

import (
	"context"
	"github.com/xelexis-dev/agent-session-broker/source/internal/codexcontrol"
)

const codexControlAvailable = false

func bindCodexControl(int) func(context.Context) (codexControl, error) {
	return func(context.Context) (codexControl, error) { return nil, codexcontrol.ErrNotDaemonHost }
}
