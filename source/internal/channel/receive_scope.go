package channel

import (
	"context"
	"crypto/sha256"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var errStaleScopeObservation = errors.New("stale receive scope observation")

var errInsufficientScope = errors.New("insufficient_scope")
var errReceiveUnavailable = errors.New("receive-unavailable: This session credential does not permit message reception.")

func insufficientScope(result *mcp.CallToolResult) bool {
	if result == nil || !result.IsError || result.StructuredContent != nil || len(result.Content) != 1 {
		return false
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	return ok && text.Text == errInsufficientScope.Error()
}
func scopeDeniedResult() *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: errInsufficientScope.Error()}}}
}

// Permission memory belongs to one local client process. Fingerprints are
// neither persisted nor logged. Keep old denials as well: replacing a token
// permits the new token, but returning to an old one must not probe it again.
type receiveScopes struct {
	mu      sync.Mutex
	entries map[[32]byte]*receivePermission
}

const receiveDecisionBound = 2 * time.Second
const (
	receiveUndecided uint32 = iota
	receiveAllowed
	receiveForbidden
)

type receivePermission struct {
	state   atomic.Uint32
	gate    chan struct{}
	decided chan struct{}
	wake    chan struct{}
}

// A real insufficient_scope is authoritative even after the long-poll timer.
func (p *receivePermission) decide(state uint32) {
	previous := p.state.Load()
	for {
		if previous == receiveForbidden || (state == receiveAllowed && previous != receiveUndecided) {
			return
		}
		if p.state.CompareAndSwap(previous, state) {
			break
		}
		previous = p.state.Load()
	}
	if previous == receiveUndecided {
		close(p.decided)
	}
	if state == receiveForbidden {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

func (o durableOptions) wrapReceive(u Upstream, credential string) Upstream {
	if _, ok := u.(*receiveUpstream); ok {
		return u
	}
	key := [32]byte{}
	if credential != "" {
		key = sha256.Sum256([]byte(credential))
	} else if source, ok := u.(interface{ receiveCredentialKey() [32]byte }); ok {
		key = source.receiveCredentialKey()
	}
	o.receive.mu.Lock()
	if o.receive.entries == nil {
		o.receive.entries = map[[32]byte]*receivePermission{}
	}
	permission := o.receive.entries[key]
	if permission == nil {
		permission = newReceivePermission(receiveUndecided)
		o.receive.entries[key] = permission
	}
	// A replacement upstream inherits the decision, not an old in-flight request's epoch.
	next := newReceivePermission(permission.state.Load())
	o.receive.entries[key] = next
	o.receive.mu.Unlock()
	return &receiveUpstream{Upstream: u, permission: next, registry: o.receive, key: key}
}

type receiveUpstream struct {
	Upstream
	permission       *receivePermission
	registry         *receiveScopes
	key              [32]byte
	observedVersion  uint64
	credentialAdvice func() string
}

func newReceivePermission(state uint32) *receivePermission {
	p := &receivePermission{gate: make(chan struct{}, 1), decided: make(chan struct{}), wake: make(chan struct{}, 1)}
	if state != receiveUndecided {
		p.state.Store(state)
		close(p.decided)
	}
	return p
}

// Only an authenticated upstream publishes observations; cached credential scopes
// seed the legacy decision but cannot reset an already decided epoch.
func (u *receiveUpstream) currentPermission() *receivePermission {
	observer, ok := u.Upstream.(interface{ receiveScopeObservation() (uint64, []string) })
	version := uint64(0)
	var scopes []string
	if ok {
		version, scopes = observer.receiveScopeObservation()
	}
	u.registry.mu.Lock()
	old := u.permission
	changed := version != 0 && version > u.observedVersion && u.registry.entries[u.key] == old
	if changed && u.observedVersion == 0 && scopes == nil {
		// The first legacy handshake learns nothing and has no prior reply to fence.
		u.observedVersion = version
		changed = false
	}
	if changed {
		state := old.state.Load()
		if scopes != nil {
			state = receiveForbidden
			if access.HasScope(scopes, access.MessageReceive) {
				state = receiveAllowed
			}
		}
		u.permission = newReceivePermission(state)
		u.observedVersion = version
		u.registry.entries[u.key] = u.permission
		select {
		case old.wake <- struct{}{}:
		default:
		}
	}
	current := u.permission
	u.registry.mu.Unlock()

	return current
}

func (u *receiveUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	for {
		result, err := u.callTool(ctx, p)
		if !errors.Is(err, errStaleScopeObservation) || ctx.Err() != nil {
			return result, err
		}
	}
}
func (u *receiveUpstream) callTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	permission := u.currentPermission()
	if p.Name != "wait_for_message" {
		return u.Upstream.CallTool(ctx, p)
	}
	// Only an undecided permission serializes callers. Scope denial happens
	// before the server's long-poll handler; a blocked wait is allowed after
	// a short bound so a LEGACY_V1 receiver cannot hold manual waits forever.
	switch permission.state.Load() {
	case receiveForbidden:
		return scopeDeniedResult(), nil
	case receiveAllowed:
		return u.receive(ctx, p, permission)
	}
	select {
	case permission.gate <- struct{}{}:
		defer func() { <-permission.gate }()
	case <-permission.decided:
		if permission.state.Load() == receiveForbidden {
			return scopeDeniedResult(), nil
		}
		return u.receive(ctx, p, permission)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch permission.state.Load() {
	case receiveForbidden:
		return scopeDeniedResult(), nil
	case receiveAllowed:
		return u.receive(ctx, p, permission)
	}
	timer := time.AfterFunc(receiveDecisionBound, func() { permission.decide(receiveAllowed) })
	defer timer.Stop()
	result, err := u.Upstream.CallTool(ctx, p)
	if err == nil && result != nil {
		if insufficientScope(result) {
			if permission != u.currentPermission() {
				return nil, errStaleScopeObservation
			}
			permission.decide(receiveForbidden)
		} else {
			permission.decide(receiveAllowed)
		}
	}
	return result, err
}
func (u *receiveUpstream) receive(ctx context.Context, p *mcp.CallToolParams, permission *receivePermission) (*mcp.CallToolResult, error) {
	result, err := u.Upstream.CallTool(ctx, p)
	if err == nil && insufficientScope(result) {
		if permission != u.currentPermission() {
			return nil, errStaleScopeObservation
		}
		permission.decide(receiveForbidden)
	}
	return result, err
}

func receiveDenied(u Upstream) bool {
	v, ok := u.(*receiveUpstream)
	return ok && v.currentPermission().state.Load() == receiveForbidden
}
func receiveWake(u Upstream) <-chan struct{} {
	if v, ok := u.(*receiveUpstream); ok {
		return v.currentPermission().wake
	}
	return nil
}
func (u *receiveUpstream) credentialRejected() bool { return upstreamRejected(u.Upstream) }
func (u *receiveUpstream) verifyActivity(ctx context.Context) bool {
	v, ok := u.Upstream.(activityUpstream)
	return ok && v.verifyActivity(ctx)
}
func (u *receiveUpstream) activityAllowed() bool {
	v, ok := u.Upstream.(activityUpstream)
	return ok && v.activityAllowed()
}
func (u *receiveUpstream) Close() error {
	if v, ok := u.Upstream.(interface{ Close() error }); ok {
		return v.Close()
	}
	return nil
}

func (u *receiveUpstream) rejectionGuidance() string {
	if u.credentialAdvice != nil {
		return u.credentialAdvice()
	}
	return ""
}

func (u *receiveUpstream) registrationSuspended() bool  { return upstreamSuspended(u.Upstream) }
func (u *receiveUpstream) clearRegistrationSuspension() { clearUpstreamSuspension(u.Upstream) }

func (u *receiveUpstream) verifyMessageAck(ctx context.Context) bool {
	v, ok := u.Upstream.(messageAckUpstream)
	return ok && v.verifyMessageAck(ctx)
}
