package channel

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type hookInput struct {
	SessionID string `json:"session_id"`
	Event     string `json:"hook_event_name"`
	TurnID    string `json:"turn_id,omitempty"`
}
type hookIdentityKey struct{}

// A request pins the exact local receiver, never just its thread's map key.
type hookIdentityPin struct {
	binding                                                                         *ingressBinding
	durable                                                                         *durableRegistration
	router                                                                          *codexIngress
	lease, session, name, harness, cwd, claim, reason                               string
	pid                                                                             int
	generation                                                                      uint64
	confirmed, claimConfirmed, ready, failed, retired, handedOver, waiting, started bool
	follow                                                                          *broker.Message
}

func pinHookIdentity(b *ingressBinding) (p hookIdentityPin) {
	if b == nil {
		return p
	}
	p = hookIdentityPin{binding: b, durable: b.durable, router: b.router, lease: b.identity.ChannelID, session: b.identity.SessionID, name: b.identity.Name, harness: b.identity.Harness, cwd: b.identity.CWD, pid: b.identity.PID, generation: b.generation, claim: b.claimThread, confirmed: b.threadConfirmed, claimConfirmed: b.claimConfirmed, ready: receiverReadyLocked(b), failed: b.failed, retired: b.retired, waiting: b.holderWaiting, started: b.started, reason: b.receiveReason, follow: b.followMessage}
	p.handedOver = b.durable != nil && b.durable.handedOver
	return p
}
func (p hookIdentityPin) eligible(thread, session string) bool {
	return p.binding != nil && thread == session && session == p.session && p.harness == "codex" && p.confirmed && (p.claim == "" || p.claimConfirmed) && !p.waiting && p.started && !p.failed && !p.retired && p.durable != nil && !p.handedOver && p.ready && p.router != nil
}

type hookIdentityObservation struct {
	request        *mcp.CallToolRequest
	observed, fast bool
	id             broker.AttachRequest
	err            error
	pin            hookIdentityPin
}

func hookObservation(ctx context.Context, req *mcp.CallToolRequest) *hookIdentityObservation {
	memo, _ := ctx.Value(hookIdentityKey{}).(*hookIdentityObservation)
	if memo == nil || memo.request != req {
		return nil
	}
	return memo
}
func hookRequestInput(req *mcp.CallToolRequest) (hookInput, bool) {
	var in hookInput
	if req == nil || req.Params == nil || req.Params.Name != "ingress_hook" {
		return in, false
	}
	return in, json.Unmarshal(req.Params.Arguments, &in) == nil
}

func (memo *hookIdentityObservation) current(b *ingressBinding) bool {
	return memo.pin == pinHookIdentity(b)
}
func observeHookIdentity(callCtx context.Context, req *mcp.CallToolRequest, mu *sync.Mutex, bindings map[string]*ingressBinding, discoverIdentity func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) (broker.AttachRequest, error) {
	memo := hookObservation(callCtx, req)
	in, hook := hookRequestInput(req)
	if memo == nil || !hook || callCtx.Value(followIdentityKey{}) != nil {
		return discoverIdentity(callCtx, req)
	}
	thread, threadErr := callerThreadID(req)
	mu.Lock()
	b := bindings[in.SessionID]
	pin := pinHookIdentity(b)
	eligible := threadErr == nil && pin.eligible(thread, in.SessionID)
	if memo.observed && memo.pin == pin && (!memo.fast || eligible) {
		id, err := memo.id, memo.err
		mu.Unlock()
		return id, err
	}
	if eligible && !memo.observed {
		memo.id, memo.err, memo.pin, memo.fast, memo.observed = b.identity, nil, pin, true, true
		id := memo.id
		mu.Unlock()
		return id, nil
	}
	mu.Unlock()
	id, err := discoverIdentity(callCtx, req)
	mu.Lock()
	// Preserve the pre-observation pin: a concurrent replacement is not proof.
	memo.id, memo.err, memo.pin, memo.fast, memo.observed = id, err, pin, false, true
	mu.Unlock()
	return id, err

}
