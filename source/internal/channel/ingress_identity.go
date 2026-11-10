package channel

import (
	"context"
	"crypto/rand"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"time"
)

func (srv *ingressServer) observeIdentity(callCtx context.Context, req *mcp.CallToolRequest) (broker.AttachRequest, error) {
	if srv.opts.identityFailure != nil {
		return broker.AttachRequest{}, srv.opts.identityFailure
	}
	identityCtx, cancel := context.WithTimeout(callCtx, 2*srv.opts.timeout)
	id, err := srv.identify(identityCtx, req)
	if err == nil && srv.opts.kimi != nil && srv.opts.kimi.checkServer != nil {
		err = srv.opts.kimi.checkServer(identityCtx)
	}
	cancel()
	if err != nil {
		return id, err
	}
	return id, err
}

func (srv *ingressServer) validateIdentity(id broker.AttachRequest) error {
	if srv.opts.tokenMode && srv.opts.device == nil && len(srv.bindings) != 0 && srv.bindings[id.SessionID] == nil {
		return errors.New("current conversation does not match this client's bound identity")
	}
	if b := srv.bindings[id.SessionID]; b != nil && (b.identity.PID != id.PID || b.identity.CWD != id.CWD || b.identity.Harness != id.Harness) {
		return errors.New("ingress parent identity changed")
	}
	return nil
}

func (srv *ingressServer) prepare(callCtx context.Context, req *mcp.CallToolRequest) (broker.AttachRequest, error) {
	id, err := srv.observeIdentity(callCtx, req)
	if err != nil {
		return id, err
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.stopping || srv.ctx.Err() != nil {
		return id, errors.New("ingress client is shutting down")
	}
	// A single session credential binds one conversation; a project token
	// lets every observed conversation obtain its own.
	if err := srv.validateIdentity(id); err != nil {
		return id, err
	}
	// Claude can publish its resumed conversation after MCP initialization.
	// Only an unregistered startup binding of this exact parent may
	// be abandoned. Never move a credential or lease between conversations.
	if _, following := callCtx.Value(followIdentityKey{}).(broker.AttachRequest); (!following || callCtx.Value(followStartupKey{}) == true) && srv.opts.device != nil && id.Harness == "claude" && srv.bindings[id.SessionID] == nil {
		for key, old := range srv.bindings {
			if old.identity.Harness != "claude" || old.identity.PID != id.PID {
				continue
			}
			// A manual explicit handover keeps the source lease and its held
			// envelope until approval and rebind have completed.
			explicit, _ := callCtx.Value(manualExplicitAttachKey{}).(bool)
			if explicit && old.started && sameCWD(old.identity.CWD, id.CWD) && old.durable != nil && old.durable.session != nil {
				old.durable.session.mu.Lock()
				manual := old.durable.session.state.LastAdmissionMode == "manual"
				old.durable.session.mu.Unlock()
				if manual {
					continue
				}
			}
			if !sameCWD(old.identity.CWD, id.CWD) || old.started || old.durable == nil || old.durable.session == nil {
				return id, errors.New("Claude parent changed after registration; reconnect this MCP client")
			}
			if old.durable.registrationID != "" {
				if callCtx.Value(followStartupKey{}) != true {
					return id, errors.New("Claude parent changed after registration; reconnect this MCP client")
				}
				// An unsuccessful, never-started resume owns saved state, not
				// a live receiver. Retire only its local lock/transport.
				if old.unlock != nil {
					old.unlock()
					old.unlock = nil
				}
				closeUpstream(old.upstream)
				delete(srv.bindings, key)
				continue
			}
			a := old.durable.session
			a.mu.Lock()
			cancel, done := a.pollCancel, a.pollDone
			a.mu.Unlock()
			if cancel != nil {
				cancel()
				waitCtx, stop := context.WithTimeout(callCtx, 2*srv.opts.timeout)
				select {
				case <-done:
					stop()
				case <-waitCtx.Done():
					stop()
					return id, errors.New("previous Claude authorization is stopping; retry shortly")
				}
			}
			// The poller's final state write has finished. Clear the abandoned
			// code on disk before releasing its lock or exposing the new ID.
			a.mu.Lock()
			a.state.Device, a.state.Credential = nil, nil
			if a.state.Registration.InitialAttempt == initialPending {
				a.state.Registration.InitialAttempt = initialStopped
			}
			a.deriveStatus(srv.opts.now())
			a.mu.Unlock()
			if err := srv.opts.persistSession(a); err != nil {
				return id, errors.New("previous Claude authorization could not be abandoned")
			}
			if old.unlock != nil {
				old.unlock()
			}
			if old.upstream != nil {
				closeUpstream(old.upstream)
			}
			delete(srv.bindings, key)
		}
	}
	var previousAuth *sessionAuth
	if b := srv.bindings[id.SessionID]; b != nil {
		if b.identity.PID != id.PID || b.identity.CWD != id.CWD || b.identity.Harness != id.Harness {
			return id, errors.New("ingress parent identity changed")
		}
		if srv.opts.device != nil && b.durable != nil {
			a := b.durable.session
			a.mu.Lock()
			keep := a.recoveryEligibleLocked(srv.opts)
			a.mu.Unlock()
			if keep {
				id.ChannelID = b.identity.ChannelID
				return id, nil
			}
		}
		if b.followMessage != nil {
			id.ChannelID = b.identity.ChannelID
			return id, nil
		}
		if !b.failed {
			id.ChannelID = b.identity.ChannelID
			return id, nil
		}
		// Never overlap the replacement with a sink or heartbeat from the
		// old lease. A later explicit registration may create a fresh binding.
		select {
		case <-b.done:
		default:
			return id, errors.New("previous ingress receiver is stopping; retry registration shortly")
		}
		if b.durable != nil {
			previousAuth = b.durable.session
		}
		if b.unlock != nil {
			b.unlock()
			b.unlock = nil
		}
		if b.upstream != nil {
			if srv.opts.state != nil && b.durable != nil {
				_ = srv.opts.state.RecordDiagnostic(b.durable.key, clientstate.DiagnosticEvent{At: time.Now().UTC(), SessionID: b.identity.SessionID, RegistrationID: b.durable.registrationID, Generation: b.generation, Event: "transport-replaced"})
			}
			closeUpstream(b.upstream)
			b.upstream = nil
		}
	} else if len(srv.bindings) >= 32 {
		return id, errors.New("too many bound ingress sessions")
	}
	// Durable Claude ownership/authorization is independent of its inbox.
	// Resume the exact stored registration before constructing that sink.
	deferNative := srv.opts.enabled && id.Harness == "claude"
	var sink func(context.Context, broker.Message) error
	var router *codexIngress
	if !deferNative {
		sink, router, err = srv.factory(id)
		if err != nil {
			return id, err
		}
	}
	binding := &ingressBinding{mu: &srv.mu, sink: sink, router: router, nativeDeferred: deferNative, receiveUnavailable: deferNative}
	id.ChannelID = rand.Text()
	if srv.opts.enabled {
		d, unlock, err := srv.opts.loadRegistrationForCall(callCtx, req, id)
		if err != nil {
			return id, srv.opts.localRegistrationFailure(id, err)
		}
		// Authorization belongs to the conversation, not to its receiver
		// generation. Preserve a pending poller's state/ownership across
		// replacement so one code never gets two concurrent poll loops.
		if previousAuth != nil && previousAuth.key == d.key {
			d.session = previousAuth
		}
		binding.durable, binding.unlock = d, unlock
		if d.pendingLease != "" {
			// A register whose answer was lost is retried with its lease so the
			// broker can answer idempotently, even from a new client process.
			id.ChannelID = d.pendingLease
		}
	}
	binding.identity = id
	if req != nil && req.Params != nil {
		if thread, e := req.Params.Meta["threadId"].(string); e && thread == id.SessionID {
			binding.threadConfirmed = true
		}
	}
	if req == nil && srv.opts.codexClaim.matches(id) {
		binding.claimThread = id.SessionID
		_ = srv.opts.state.RecordDiagnostic(binding.durable.key, clientstate.DiagnosticEvent{At: time.Now().UTC(), SessionID: id.SessionID, RegistrationID: binding.durable.registrationID, Event: "codex-claim-unconfirmed"})
	}
	if router != nil {
		if router.steer != nil {
			if srv.opts.state != nil && codexControlAvailable {
				router.recovery = &steerRecovery{store: srv.opts.state, scope: srv.opts.scope, thread: id.SessionID}
			}
			router.steer.confirmed = func() bool {
				srv.mu.Lock()
				defer srv.mu.Unlock()
				return !binding.retired && (binding.threadConfirmed || binding.claimConfirmed) && binding.identity.SessionID == id.SessionID
			}
			router.steer.gate = func() string {
				srv.mu.Lock()
				defer srv.mu.Unlock()
				if srv.opts.codexSteerOff {
					return "off"
				}
				if !(binding.threadConfirmed || binding.claimConfirmed) {
					return "unconfirmed-thread"
				}
				if binding.messageAck == nil || binding.messageAck.lease != binding.identity.ChannelID || binding.failed || binding.retired {
					return "ack-required"
				}
				return ""
			}
			router.steer.diagnose = func(code string) { srv.opts.diagnose(binding, code, "", "") }
		}
		router.onRecovery = func() { srv.opts.diagnose(binding, "codex-stop-missing", "", "") }
	}
	srv.bindings[id.SessionID] = binding
	if srv.opts.kimi != nil {
		srv.opts.kimi.mu.Lock()
		srv.opts.kimi.fence = func() {
			srv.mu.Lock()
			binding.failed = true
			if binding.cancel != nil {
				binding.cancel()
			}
			srv.mu.Unlock()
		}
		srv.opts.kimi.handedOff = func() { srv.opts.diagnose(binding, "kimi-hook-handoff", "", "") }
		srv.opts.kimi.mu.Unlock()
	}
	if srv.opts.bindingCreated != nil {
		srv.opts.bindingCreated(binding)
	}
	return id, nil
}
