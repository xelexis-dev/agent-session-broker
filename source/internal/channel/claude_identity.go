package channel

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

const claudeInboxRecovery = "reconnect-required: This session is registered, but its Claude native inbox is unavailable. Only list_sessions and get_session are available; sending and receiving are blocked. Use /mcp reconnect to refresh this MCP client; do not request a new approval code."

// Startup stabilization is retained. Auto-mode bindings subsequently follow
// the same parent process through the serialized controller installed by ingress.
func claudeConversationIdentity(initial broker.AttachRequest, opts *durableOptions) func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
	observe := opts.observe
	if opts.device == nil && opts.claudeInitial != nil {
		return func(ctx context.Context, _ *mcp.CallToolRequest) (broker.AttachRequest, error) {
			return opts.claudeInitial(ctx)
		}
	}
	if (opts.device == nil && opts.claudeInitial == nil) || observe == nil {
		return func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) { return initial, nil }
	}
	var follow *claudeFollow
	if opts.claudeInitial != nil {
		follow = &claudeFollow{initial: initial, opts: opts, began: followPollClock(opts).now()}
	} else {
		follow = newClaudeFollow(initial, opts)
	}
	opts.claudeFollow = follow
	var mu sync.Mutex
	var binding *ingressBinding
	var pinned *broker.AttachRequest
	startup := make(chan struct{}, 1)
	settled := false
	previous := opts.bindingCreated
	opts.bindingCreated = func(b *ingressBinding) {
		if previous != nil {
			previous(b)
		}
		// Called with b.mu held. Do not take the binding lock here.
		if b.identity.Harness == "claude" && b.identity.PID == initial.PID && sameCWD(b.identity.CWD, initial.CWD) {
			mu.Lock()
			binding = b
			mu.Unlock()
		}
	}
	registered := func() (broker.AttachRequest, bool) {
		mu.Lock()
		b, cached := binding, pinned
		mu.Unlock()
		var id broker.AttachRequest
		started := false
		if b != nil {
			b.mu.Lock()
			started, id = b.started, b.identity
			d := b.durable
			b.mu.Unlock()
			if !started && d != nil && d.session != nil {
				d.session.mu.Lock()
				pending := d.session.state.Handover != nil && d.session.state.Handover.Manual && d.session.state.Registration.InitialAttempt == initialPending
				d.session.mu.Unlock()
				if pending {
					return broker.AttachRequest{}, false
				}
			}
		}
		if cached != nil {
			return *cached, true
		}
		if !started {
			return broker.AttachRequest{}, false
		}
		mu.Lock()
		if pinned == nil {
			pinned = &id
		}
		id = *pinned
		mu.Unlock()
		return id, true
	}
	return func(ctx context.Context, _ *mcp.CallToolRequest) (broker.AttachRequest, error) {
		if id, ok := ctx.Value(followIdentityKey{}).(broker.AttachRequest); ok {
			return id, nil
		}
		if id, ok := registered(); ok {
			return follow.check(ctx, id)
		}
		follow.mu.Lock()
		defer follow.mu.Unlock()
		if initial.SessionID == "" && opts.claudeInitial != nil {
			resolved, e := opts.claudeInitial(ctx)
			if e != nil {
				return broker.AttachRequest{}, e
			}
			if resolved.Harness != "claude" || resolved.PID != initial.PID || resolved.SessionID == "" || resolved.CWD == "" {
				return broker.AttachRequest{}, errors.New("current Claude parent identity is unavailable")
			}
			initial = resolved
			follow.initial = resolved
			follow.key = opts.handoverProcessKey(resolved)
		}
		select {
		case startup <- struct{}{}:
		case <-ctx.Done():
			return broker.AttachRequest{}, ctx.Err()
		}
		defer func() { <-startup }()
		if id, ok := registered(); ok {
			return id, nil
		}
		var current broker.AttachRequest
		var err error
		if !settled {
			current, err = settleClaudeStartup(ctx, initial, observe, opts.claudeStartupClock)
			if err == nil {
				settled = true
			}
		} else {
			current, err = observe(ctx, initial)
		}
		// Registration may have completed while the observer was running. Prefer
		// the committed binding even if this observation failed or changed ID.
		if id, ok := registered(); ok {
			return id, nil
		}
		if err != nil || current.Harness != "claude" || current.PID != initial.PID || !sameCWD(current.CWD, initial.CWD) || current.SessionID == "" {
			return broker.AttachRequest{}, errors.New("current Claude parent identity is unavailable")
		}
		return current, nil
	}
}

// A brief startup grace avoids approving a resume placeholder. Stability is a
// bounded observation, not a claim that a producer can never publish later.
// No credential lookup or device request happens until this succeeds.
func settleClaudeStartup(ctx context.Context, initial broker.AttachRequest, observe func(context.Context, broker.AttachRequest) (broker.AttachRequest, error), clock devicePollClock) (broker.AttachRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if clock.now == nil || clock.sleep == nil {
		clock = realPollClock()
	}
	began, quiet := clock.now(), clock.now()
	last := ""

	for {
		current, err := observe(ctx, initial)
		if err == nil {
			if current.Harness != "claude" || current.PID != initial.PID || !sameCWD(current.CWD, initial.CWD) || current.SessionID == "" {
				return broker.AttachRequest{}, errors.New("current Claude parent identity is unavailable")
			}
			if current.SessionID != last {
				last, quiet = current.SessionID, clock.now()
			}
			if clock.now().Sub(began) >= 2*time.Second && clock.now().Sub(quiet) >= 750*time.Millisecond {
				return current, nil
			}
		} else {
			last = ""
			quiet = clock.now()
		}
		if clock.now().Sub(began) >= 5*time.Second || clock.sleep(ctx, 100*time.Millisecond) != nil {
			return broker.AttachRequest{}, errors.New("current Claude parent identity did not stabilize; reconnect this MCP client")
		}
	}
}
