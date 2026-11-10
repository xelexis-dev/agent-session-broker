package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"

	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

var ErrSessionNotFound = errors.New("claude session not found")

type Call struct {
	Args []string
	CWD  string
	Env  map[string]string
}
type Executor interface {
	Run(context.Context, Call) (string, error)
}

type livenessProbe interface {
	Alive(pid int) (bool, error)
}
type CLIExecutor struct{}

func (*CLIExecutor) Run(ctx context.Context, c Call) (string, error) {
	cmd := exec.CommandContext(ctx, "claude", c.Args...)
	cmd.Dir = c.CWD
	cmd.Env = os.Environ()
	for k, v := range c.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", errors.New(strings.TrimSpace(stderr.String() + " " + err.Error()))
	}
	return stdout.String(), nil
}

type Adapter struct {
	exec     Executor
	liveness livenessProbe
}

type sanitizedError struct {
	message string
	causes  []error
}

func (e *sanitizedError) Error() string   { return e.message }
func (e *sanitizedError) Unwrap() []error { return e.causes }

func NewAdapter(e Executor) *Adapter {
	return &Adapter{exec: e, liveness: defaultLivenessProbe{}}
}

type rawSession struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	State     string `json:"state"`
	PID       int    `json:"pid"`
	CWD       string `json:"cwd"`
}

func (r rawSession) normalizedStatus() broker.SessionStatus {
	// A dialog/tool permission prompt keeps the interactive process alive.
	// It may queue incoming work, but is not available as an idle worker.
	if r.Kind == "interactive" && (r.Status == "busy" || r.Status == "waiting") && r.State == "" {
		return broker.StatusWorking
	}
	if r.State != "" {
		return normalizeStatus(r.State)
	}
	return normalizeStatus(r.Status)
}

func (a *Adapter) statusWithProcessLiveness(pid int, rosterStatus broker.SessionStatus) broker.SessionStatus {
	if pid <= 0 {
		return broker.StatusStalled
	}
	alive, err := a.liveness.Alive(pid)
	if err != nil {
		return broker.StatusStalled
	}
	if !alive {
		return broker.StatusMissing
	}
	return rosterStatus
}

func (a *Adapter) rawSessions(ctx context.Context) ([]rawSession, error) {
	out, err := a.exec.Run(ctx, Call{Args: []string{"agents", "--json", "--all"}})
	if err != nil {
		return nil, err
	}
	var raw []rawSession
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, err
	}
	for i := range raw {
		if raw[i].Kind == "interactive" && raw[i].ID == "" && validCanonicalUUID(raw[i].SessionID) {
			raw[i].ID = raw[i].SessionID
		}
	}
	return raw, nil
}

func sanitizeError(message string, causes ...error) error {
	nonNil := causes[:0]
	for _, cause := range causes {
		if cause != nil {
			nonNil = append(nonNil, cause)
		}
	}
	if len(nonNil) == 0 {
		return errors.New(message)
	}
	return &sanitizedError{message: message, causes: nonNil}
}

func validCanonicalUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func normalizeStatus(status string) broker.SessionStatus {
	switch status {
	case "working":
		return broker.StatusWorking
	case "idle", "blocked":
		return broker.StatusIdle
	case "alive":
		return broker.StatusAlive
	case "starting":
		return broker.StatusStarting
	case "stopping":
		return broker.StatusStopping
	case "", "done", "stopped":
		return broker.StatusMissing
	default:
		return broker.StatusStalled
	}
}
