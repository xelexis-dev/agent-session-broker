//go:build darwin || linux

package channel

import (
	"context"
	"errors"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/codexcontrol"
)

type failedResumeControl struct {
	fakeResumeControl
	err error
}

func (c *failedResumeControl) Loaded(context.Context) (bool, error) { return false, c.err }

func TestDaemonResumeUnsupportedAndUnverifiedStopWithoutRetry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		reason string
		waits  int
	}{
		{"old-codex", &codexcontrol.RPCError{Code: -32601}, "unsupported", 0},
		{"pin-failed", codexcontrol.ErrUnverifiedHost, "unverified-host", 0},
		{"transient", errors.New("temporary transport failure"), "stopped", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &failedResumeControl{err: tc.err}
			waits, dials := 0, 0
			got := runDaemonResume(t.Context(), daemonResumeDeps{
				alive:      func() bool { return true },
				dial:       func(context.Context) (daemonResumeClient, error) { dials++; return c, nil },
				wait:       func(context.Context) error { waits++; return context.Canceled },
				stopReason: codexResumeStopReason,
			})
			if got != tc.reason || waits != tc.waits || dials != 1 || c.calls != 0 {
				t.Fatalf("reason=%s waits=%d dials=%d auth=%d", got, waits, dials, c.calls)
			}
		})
	}
}
