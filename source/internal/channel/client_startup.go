package channel

import (
	"context"
	"errors"
	"sync"

	"github.com/xelexis-dev/agent-session-broker/source/internal/claude"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// Only cheap process ancestry is needed before serving the local catalog.
type clientParentSource struct {
	ancestors        func() ([]int, error)
	classify         func(int) (string, error)
	classifyObserved func(int) (string, string, error)
	inspectClaude    func(context.Context, int) (broker.AttachRequest, error)
	processKey       func(context.Context, int) (string, error)
}

func (p clientParentSource) withDefaults() clientParentSource {
	if p.ancestors == nil {
		p.ancestors = ancestorPIDs
	}
	if p.classify == nil {
		p.classify = processHarness
		p.classifyObserved = processHarnessObserved
	}
	if p.inspectClaude == nil {
		p.inspectClaude = func(ctx context.Context, pid int) (broker.AttachRequest, error) {
			return claudeParentIdentity(ctx, &claude.CLIExecutor{}, pid)
		}
	}
	if p.processKey == nil {
		p.processKey = claudeParentProcessKey
	}
	return p
}
func cachedClaudeParent(pid int, inspect func(context.Context, int) (broker.AttachRequest, error)) func(context.Context) (broker.AttachRequest, error) {
	var mu sync.Mutex
	var initial broker.AttachRequest
	return func(ctx context.Context) (broker.AttachRequest, error) {
		mu.Lock()
		defer mu.Unlock()
		if initial.SessionID != "" {
			return initial, nil
		}
		id, err := inspect(ctx, pid)
		if err != nil {
			return broker.AttachRequest{}, err
		}
		if id.Harness != "claude" || id.PID != pid || id.SessionID == "" || id.CWD == "" {
			return broker.AttachRequest{}, errors.New("current Claude parent identity is unavailable")
		}
		initial = id
		return initial, nil
	}
}
func deferredClaudeInbox(ctx context.Context, initial func(context.Context) (broker.AttachRequest, error), observe func(context.Context, broker.AttachRequest) (broker.AttachRequest, error), key func(context.Context, int) (string, error)) ingressSinkFactory {
	var mu sync.Mutex
	var factory ingressSinkFactory
	return func(id broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
		mu.Lock()
		if factory == nil {
			first, err := initial(ctx)
			if err != nil {
				mu.Unlock()
				return nil, nil, err
			}
			factory = newClaudeInboxFactory(ctx, first, observe, key)
		}
		current := factory
		mu.Unlock()
		return current(id)
	}
}
