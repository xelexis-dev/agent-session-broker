package claude

import (
	"context"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

type fakeExecutor struct {
	calls   []Call
	outputs []string
	errors  []error
}

type fakeLivenessProbe struct {
	alive map[int]bool
	errs  map[int]error
}

func (p fakeLivenessProbe) Alive(pid int) (bool, error) {
	return p.alive[pid], p.errs[pid]
}

func (f *fakeExecutor) Run(_ context.Context, c Call) (string, error) {
	f.calls = append(f.calls, c)
	var output string
	if len(f.outputs) != 0 {
		output = f.outputs[0]
		f.outputs = f.outputs[1:]
	}
	var err error
	if len(f.errors) != 0 {
		err = f.errors[0]
		f.errors = f.errors[1:]
	}
	return output, err
}

func TestNormalizeStatusFailsClosedForUnknownClaudeStatus(t *testing.T) {
	if got := normalizeStatus("crashed"); got != broker.StatusStalled {
		t.Fatalf("normalizeStatus(crashed) = %q, want %q", got, broker.StatusStalled)
	}
}
