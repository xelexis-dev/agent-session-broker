//go:build darwin || linux

package channel

import (
	"context"
	"errors"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/codexcontrol"
)

func runNativeCodexResumeHelper(ctx context.Context, o daemonResumeOptions) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	source := resumeProcessSource{
		birth: func(pid int) (string, error) {
			started, err := codexClaimProcessStart(pid)
			if err != nil {
				return "", err
			}
			return started.UTC().Format(time.RFC3339Nano), nil
		},
		key: kimiProcessKey, codex: func(pid int) bool { h, e := processHarness(pid); return e == nil && h == "codex" }, now: time.Now, wait: resumeWait,
	}
	alive, err := pinResumeTUI(ctx, o.pid, source)
	if err != nil {
		return ErrCodexResumeIncomplete
	}
	launcher, err := codexcontrol.BindLauncher(o.home, o.socket, o.inode, o.thread, kimiProcessKey)
	if err != nil {
		return ErrCodexResumeIncomplete
	}
	// Cancellation reaches an in-flight RPC when the TUI exits or its key changes.
	watched, stop := context.WithCancel(ctx)
	defer stop()
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		for {
			if resumeWait(watched) != nil {
				return
			}
			if !alive() {
				stop()
				return
			}
		}
	}()
	reason := runDaemonResume(watched, daemonResumeDeps{alive: alive, wait: resumeWait, dial: func(c context.Context) (daemonResumeClient, error) { return launcher.Dial(c) }, stopReason: codexResumeStopReason})
	stop()
	<-joined
	if reason == "ready" {
		return nil
	}
	return ErrCodexResumeIncomplete
}

func codexResumeStopReason(err error) string {
	if errors.Is(err, codexcontrol.ErrUnverifiedHost) {
		return "unverified-host"
	}
	var rpc *codexcontrol.RPCError
	if errors.As(err, &rpc) && rpc.Code == -32601 {
		return "unsupported"
	}
	return ""
}
