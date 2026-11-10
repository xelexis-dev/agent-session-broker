package channel

import (
	"context"
	"os"

	"github.com/xelexis-dev/agent-session-broker/source/internal/nativeinbox"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// The inbox belongs to the live Claude process, while credentials belong to
// exact conversation keys. Real 2.1.300 /resume and /clear evidence shows that
// the process keeps its initial inherited inbox after its conversation changes.
// The startup env UUID may precede resume. Initial identity agreement or a
// verified socket peer PID proves inbox provenance; current parent checks remain.
func newClaudeInboxFactory(ctx context.Context, initial broker.AttachRequest, observe func(context.Context, broker.AttachRequest) (broker.AttachRequest, error), processKey func(context.Context, int) (string, error)) ingressSinkFactory {
	envID := os.Getenv("CLAUDE_CODE_SESSION_ID")
	envSocket, envToken := os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"), os.Getenv("CLAUDE_CODE_MESSAGING_TOKEN")
	initialKey, keyErr := processKey(ctx, initial.PID)
	proven := initial.Harness == "claude" && keyErr == nil && initialKey != "" && observe != nil
	verify := func(ctx context.Context, id broker.AttachRequest) bool {
		if !proven || id.Harness != "claude" || id.PID != initial.PID || !sameCWD(id.CWD, initial.CWD) || !parentSessionIDPattern.MatchString(id.SessionID) || os.Getenv("CLAUDE_CODE_SESSION_ID") != envID || os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET") != envSocket || os.Getenv("CLAUDE_CODE_MESSAGING_TOKEN") != envToken {
			return false
		}
		before, err := processKey(ctx, initial.PID)
		if err != nil || before != initialKey {
			return false
		}
		current, err := observe(ctx, initial)
		if err != nil || current.Harness != id.Harness || current.PID != id.PID || current.SessionID != id.SessionID || !sameCWD(current.CWD, id.CWD) {
			return false
		}
		after, err := processKey(ctx, initial.PID)
		return err == nil && after == initialKey
	}
	return func(id broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
		if id.Harness != "claude" || id.PID != initial.PID || !sameCWD(id.CWD, initial.CWD) {
			return nil, nil, nativeinbox.ErrUnavailable
		}
		if !proven {
			// Without process provenance retain the strict inherited-ID behavior.
			p, err := nativeinbox.FromEnvironment(id.SessionID)
			if err != nil {
				return nil, nil, err
			}
			return p.Post, nil, nil
		}
		if !verify(ctx, id) {
			return nil, nil, nativeinbox.ErrUnavailable
		}
		var p *nativeinbox.Poster
		var err error
		if initial.SessionID == envID {
			p, err = nativeinbox.FromEnvironment(envID)
		} else {
			p, err = nativeinbox.FromParentEnvironment(initial.PID, func(callCtx context.Context) bool { return verify(callCtx, id) })
		}
		if err != nil {
			return nil, nil, err
		}
		return func(callCtx context.Context, m broker.Message) error {
			if !verify(callCtx, id) {
				return nativeinbox.ErrUnavailable
			}
			return p.Post(callCtx, m)
		}, nil, nil
	}
}
