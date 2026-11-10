package channel

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"path/filepath"
	"regexp"
	"time"
)

type daemonResumeOptions struct {
	thread, home, socket string
	pid                  int
	inode                uint64
}
type daemonResumeClient interface {
	Loaded(context.Context) (bool, error)
	Connected(context.Context) (bool, error)
	AuthStatus(context.Context) (json.RawMessage, error)
	Close() error
}
type daemonResumeDeps struct {
	alive      func() bool
	dial       func(context.Context) (daemonResumeClient, error)
	wait       func(context.Context) error
	stopReason func(error) string
}

func runDaemonResume(ctx context.Context, d daemonResumeDeps) string {
	calls := 0
	for ctx.Err() == nil && d.alive() {
		c, err := d.dial(ctx)
		reason := ""
		if err == nil {
			loaded, e := c.Loaded(ctx)
			err = e
			if err == nil && loaded && d.alive() {
				connected, e := c.Connected(ctx)
				err = e
				if err == nil && connected && d.alive() {
					calls++
					raw, e := c.AuthStatus(ctx)
					err = e
					if err == nil {
						reason = resumeAuthResult(raw)
					}
				}
			}
			c.Close()
		}
		if reason == "ready" || reason == "not-registered" || reason == "unverified" {
			return reason
		}
		if err != nil && d.stopReason != nil {
			if stop := d.stopReason(err); stop != "" {
				return stop
			}
		}
		if calls >= 3 {
			return "attempts-exhausted"
		}
		if !d.alive() || d.wait(ctx) != nil {
			break
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	return "stopped"
}
func resumeAuthResult(raw json.RawMessage) string {
	var response struct {
		IsError    bool                          `json:"isError"`
		Structured json.RawMessage               `json:"structuredContent"`
		Content    []struct{ Type, Text string } `json:"content"`
	}
	if json.Unmarshal(raw, &response) != nil || response.IsError {
		return "transient"
	}
	data := response.Structured
	if len(data) == 0 || string(data) == "null" {
		for _, item := range response.Content {
			if item.Type == "text" {
				data = []byte(item.Text)
				break
			}
		}
	}
	var status struct {
		Registered *bool `json:"registered"`
		Ready      *bool `json:"receiverReady"`
	}
	if json.Unmarshal(data, &status) != nil || status.Registered == nil {
		return "unverified"
	}
	if !*status.Registered {
		return "not-registered"
	}
	if status.Ready == nil {
		return "unverified"
	}
	if *status.Ready {
		return "ready"
	}
	return "not-ready"
}
func parseDaemonResumeOptions(args []string) (daemonResumeOptions, error) {
	var o daemonResumeOptions
	f := flag.NewFlagSet("codex-resume-helper", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&o.thread, "thread", "", "exact Codex thread UUID")
	f.StringVar(&o.home, "codex-home", "", "Codex home")
	f.StringVar(&o.socket, "socket", "", "canonical Unix socket")
	f.IntVar(&o.pid, "pid", 0, "launcher PID")
	f.Uint64Var(&o.inode, "inode", 0, "socket inode")
	if f.Parse(args) != nil || f.NArg() != 0 || !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(o.thread) || o.pid <= 0 || o.inode == 0 || !filepath.IsAbs(o.home) || !filepath.IsAbs(o.socket) {
		return o, errors.New("invalid codex-resume-helper options")
	}
	return o, nil
}
func resumeWait(ctx context.Context) error {
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var ErrCodexResumeIncomplete = errors.New("Codex resume readiness was not confirmed")

func RunCodexResumeHelper(ctx context.Context, args []string) error {
	o, err := parseDaemonResumeOptions(args)
	if err != nil {
		return err
	}
	return runNativeCodexResumeHelper(ctx, o)
}

type resumeProcessSource struct {
	birth, key func(int) (string, error)
	codex      func(int) bool
	now        func() time.Time
	wait       func(context.Context) error
}

// Birth identity survives launcher exec; the full key is pinned only in Codex.
func pinResumeTUI(ctx context.Context, pid int, s resumeProcessSource) (func() bool, error) {
	birth, err := s.birth(pid)
	if err != nil || birth == "" {
		return nil, ErrCodexResumeIncomplete
	}
	deadline := s.now().Add(10 * time.Second)
	for ctx.Err() == nil && s.now().Before(deadline) {
		current, err := s.birth(pid)
		if err != nil || current != birth {
			return nil, ErrCodexResumeIncomplete
		}
		if s.codex(pid) {
			key, err := s.key(pid)
			if err != nil || key == "" {
				return nil, ErrCodexResumeIncomplete
			}
			alive := func() bool {
				b, e := s.birth(pid)
				k, f := s.key(pid)
				return e == nil && f == nil && b == birth && k == key
			}
			if !alive() {
				return nil, ErrCodexResumeIncomplete
			}
			return alive, nil
		}
		if s.wait(ctx) != nil {
			break
		}
	}
	return nil, ErrCodexResumeIncomplete
}
