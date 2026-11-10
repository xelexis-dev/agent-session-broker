package channel

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"time"
)

func (srv *ingressServer) deviceStatus(b *ingressBinding) string {
	a := b.durable.session
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

func (srv *ingressServer) credentialName(b *ingressBinding) string {
	a := b.durable.session
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state.Credential == nil {
		return ""
	}
	return a.state.Credential.Nickname
}

func (srv *ingressServer) ensureUpstream(b *ingressBinding) Upstream {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if b.durable.handedOver {
		return nil
	}
	a := b.durable.session
	a.mu.Lock()
	if a.state.Credential != nil && a.state.Credential.Scopes != nil && access.RestrictedScopes(a.state.Credential.Scopes) && !a.scopesVerified {
		a.mu.Unlock()
		return nil
	}
	credential := ""
	var scopes []string
	if a.state.Credential != nil {
		credential = a.state.Credential.AccessToken
		scopes = a.state.Credential.Scopes
	}
	a.mu.Unlock()
	// An upstream keeps its own bearer and its sticky rejection, so a
	// refused or replaced credential must never reach the broker again: a
	// new approval gets a new connection.
	if b.upstream != nil && b.upstreamCredential != credential {
		if srv.opts.state != nil {
			_ = srv.opts.state.RecordDiagnostic(b.durable.key, clientstate.DiagnosticEvent{At: time.Now().UTC(), SessionID: b.identity.SessionID, RegistrationID: b.durable.registrationID, Generation: b.generation, Event: "transport-replaced"})
		}
		closeUpstream(b.upstream)
		b.upstream = nil
	}
	if b.upstream != nil {
		return b.upstream
	}
	if credential == "" || srv.stopping {
		return nil
	}
	rawUpstream := srv.opts.device.newUpstream(credential)
	if actual, ok := rawUpstream.(*brokerUpstream); ok {
		a.mu.Lock()
		sameAdmission := a.state.AdmissionFingerprint == srv.opts.admissionFingerprint()
		a.mu.Unlock()
		if b.durable.registrationID != "" {
			actual.expectedProjectGeneration = actual.projectState.generation(actual.projectSource)
		}
		if sameAdmission {
			actual.projectParent = srv.opts.admissionFingerprint()
			actual.projectState.inherit(actual.projectSource, actual.projectParent)
		}
	}
	b.upstream = srv.opts.wrapReceive(rawUpstream, credential)
	b.upstreamCredential = credential

	b.upstream.(*receiveUpstream).credentialAdvice = func() string {
		if receiverRetired(b) {
			return a.pendingText()
		}
		srv.opts.diagnose(b, "reconnect-failed", "authorization", "authorization")
		srv.opts.rejectCredentialIfCurrent(a, credential)
		srv.startRecovery(b)
		return a.pendingText()
	}
	if scopes != nil {
		permission := b.upstream.(*receiveUpstream).permission
		allowed := false
		for _, scope := range scopes {
			if scope == "message:receive" {
				allowed = true
			}
		}
		if allowed {
			permission.decide(receiveAllowed)
		} else {
			permission.decide(receiveForbidden)
		}
	}
	if srv.opts.state != nil {
		_ = srv.opts.state.RecordDiagnostic(b.durable.key, clientstate.DiagnosticEvent{At: time.Now().UTC(), SessionID: b.identity.SessionID, RegistrationID: b.durable.registrationID, Generation: b.generation, Event: "transport-created"})
	}

	if srv.opts.connected != nil {
		srv.opts.connected(b.upstream)
	}
	return b.upstream
}

func (srv *ingressServer) startPolling(b *ingressBinding) {
	a := b.durable.session
	a.mu.Lock()
	if a.polling || a.status != authPending {
		a.mu.Unlock()
		return
	}
	pollCtx, pollCancel := context.WithCancel(srv.ctx)
	pollDone := make(chan struct{})
	a.polling, a.pollCancel, a.pollDone = true, pollCancel, pollDone
	a.mu.Unlock()
	srv.mu.Lock()
	if srv.stopping || srv.ctx.Err() != nil {
		srv.mu.Unlock()
		a.mu.Lock()
		a.polling = false
		close(pollDone)
		pollCancel()
		a.mu.Unlock()
		return
	}
	srv.workers.Add(1)
	sessionID := b.identity.SessionID
	srv.mu.Unlock()
	go func() {
		defer srv.workers.Done()
		// The wait belongs to the conversation, not to the tool call that
		// started it; shutdown, expiry or a verified Claude ID switch ends it.
		defer pollCancel()
		granted := srv.opts.awaitDeviceApproval(pollCtx, a)
		a.mu.Lock()
		a.polling = false
		close(pollDone)
		a.mu.Unlock()
		if granted && pollCtx.Err() == nil {
			if err := srv.autoAttach(pollCtx, deviceRequest(sessionID)); err != nil {
				if receiverRetired(b) {
					return
				}
				srv.opts.diagnose(b, "reconnect-failed", "other", diagnosticErrorClass(err, false))
				// Reapproval can finish after rejection became visible but
				// before the old receiver finished stopping. Retire only that
				// binding; never overlap its lease with a replacement receiver.
				srv.mu.Lock()
				a.mu.Lock()
				replaced := a.status == authAuthorized && a.state.Credential != nil && a.state.Credential.AccessToken != b.upstreamCredential
				a.mu.Unlock()
				var done <-chan struct{}
				if srv.bindings[sessionID] == b && b.started && replaced {
					b.failed = true
					b.cancel()
					done = b.done
				}
				srv.mu.Unlock()
				if done == nil {
					return
				}
				waitCtx, cancel := context.WithTimeout(srv.ctx, 2*srv.opts.timeout)
				defer cancel()
				select {
				case <-waitCtx.Done():
					if receiverRetired(b) {
						return
					}
					srv.opts.diagnose(b, "reconnect-failed", diagnosticErrorClass(waitCtx.Err(), false), diagnosticErrorClass(waitCtx.Err(), false))
					return
				case <-done:
				}
				// One causal retry, owned by the client rather than a tool call.
				if err := srv.autoAttach(waitCtx, deviceRequest(sessionID)); err != nil {
					if receiverRetired(b) {
						return
					}
					srv.opts.diagnose(b, "reconnect-failed", "other", diagnosticErrorClass(err, false))
				}
			}
		}
	}()
}

func (srv *ingressServer) beginAuthorization(callCtx context.Context, b *ingressBinding, identity broker.AttachRequest) error {
	a := b.durable.session
	a.exchangeMu.Lock()
	defer a.exchangeMu.Unlock()
	a.mu.Lock()
	authorized := a.status == authAuthorized
	pending := a.status == authPending
	a.mu.Unlock()
	if authorized {
		return nil
	}
	if pending {
		return errAuthorizationPending
	}
	if err := srv.opts.transitionAdmission(a); err != nil {
		return err
	}
	if err := srv.opts.resetExplicitAdmission(a); err != nil {
		return err
	}
	a.mu.Lock()
	manual := a.state.Handover != nil && a.state.Handover.Manual
	manualName := ""
	if manual {
		manualName = temporaryHandoverName(a.state.Handover.Registration.Name)
	}
	a.mu.Unlock()
	if manual {
		identity.Name = manualName
		if err := srv.opts.requestDeviceCode(callCtx, a, identity); err != nil {
			return err
		}
		srv.startPolling(b)
		return errAuthorizationPending
	}
	b.registerMu.Lock()
	handled, err := srv.opts.tryDirect(callCtx, a, identity)
	if handled && err == nil {
		srv.mu.Lock()
		a.mu.Lock()
		saved := a.state.Registration
		b.durable.name, b.durable.revision = saved.Name, saved.Revision
		b.durable.registrationID, b.durable.detached = saved.RegistrationID, saved.Detached
		b.durable.initialAttempt = saved.InitialAttempt
		b.durable.pendingRequest, b.durable.pendingLease, b.durable.pendingRevision = saved.PendingRequestID, saved.PendingLease, saved.PendingRevision
		if saved.RegistrationID == "" {
			b.pendingResume = broker.ResumeRequest{}
		}
		a.mu.Unlock()
		srv.mu.Unlock()
	}
	b.registerMu.Unlock()
	if handled {
		if err != nil {
			a.mu.Lock()
			a.status = authFailed
			a.message = err.Error()
			a.mu.Unlock()
			if initialPermanent(err) {
				srv.opts.stopAutoRecovery(a, recoveryStopReason(err))
				srv.opts.stopInitialRegistration(a)
			}
		}
		return err
	}
	if err := srv.opts.requestDeviceCode(callCtx, b.durable.session, identity); err != nil {
		return err
	}
	srv.startPolling(b)
	return errAuthorizationPending
}

func (srv *ingressServer) routeDeviceCall(callCtx context.Context, req *mcp.CallToolRequest) (Upstream, string, error) {
	id, err := srv.identify(callCtx, req)
	if err != nil {
		return nil, "", err
	}
	srv.mu.Lock()
	b := srv.bindings[id.SessionID]
	srv.mu.Unlock()
	if b == nil || b.durable == nil || !srv.opts.automaticRegistrationAllowed(b) {
		if b != nil && b.durable != nil {
			return nil, b.durable.session.pendingText(), nil
		}
		return nil, conversationNotRegistered, nil
	}
	a := b.durable.session
	a.mu.Lock()
	blocked := a.automaticStoppedLocked() || a.recovering || (a.state.AutoRecovery != nil && a.state.AutoRecovery.StopReason != "")
	a.mu.Unlock()
	if blocked {
		return nil, a.pendingText(), nil
	}
	if target := srv.ensureUpstream(b); target != nil {
		if req.Params.Name == "send_message" {
			srv.mu.Lock()
			b.used = true
			srv.mu.Unlock()
		}
		return target, "", nil
	}
	return nil, b.durable.session.pendingText(), nil
}

func (srv *ingressServer) deviceAuthStatus(callCtx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, authStatus, error) {
	id, err := srv.identify(callCtx, req)
	if err != nil {
		return nil, authStatus{}, err
	}
	srv.mu.Lock()
	b := srv.bindings[id.SessionID]
	if b == nil || b.durable == nil {
		srv.mu.Unlock()
		return nil, authVersions(srv.ctx, nil, srv.opts.deviceIdentityStatus(authStatus{Status: authUnauthorized, Message: "this conversation has not started authorization yet"})), nil
	}
	registered := b.started && !b.failed
	name, registrationID := b.identity.Name, b.durable.registrationID
	target := b.upstream
	receiveReason := b.receiveReason
	receiveUnavailable, receiveGate := b.receiveUnavailable, b.receiveGate
	srv.mu.Unlock()
	if !registered {
		name = ""
	}
	out := b.durable.session.report(registered, name, registrationID)
	out.Diagnostics = srv.opts.diagnostics(b)
	if receiveReason != "" {
		out.ReceiveError = receiveReason
		if receiveUnavailable {
			out.Message = claudeInboxRecovery
		} else if receiveGate == "follow" {
			out.Message = "This session is registered, but sending is blocked by local resume-follow verification."
		} else {
			out.Message = receiveReason
		}
	}
	out = authScopes(callCtx, target, out)
	// The diagnostic read itself may observe 401; publish the resulting local
	// state rather than returning the pre-read registered/authorized snapshot.
	srv.mu.Lock()
	registered = b.started && !b.failed
	name = b.identity.Name
	srv.mu.Unlock()
	if !registered {
		name = ""
	}
	fresh := b.durable.session.report(registered, name, registrationID)
	out.Status, out.Registered, out.Name = fresh.Status, fresh.Registered, fresh.Name
	if receiveReason == "" {
		out.Message = fresh.Message
	}
	out.Diagnostics = srv.opts.diagnostics(b)
	srv.mu.Lock()
	detached := b.resumeStopReason == "registration-detached" || b.durable.detached
	srv.mu.Unlock()
	b.durable.session.mu.Lock()
	explicit := b.durable.session.explicitAttaching > 0
	b.durable.session.mu.Unlock()
	if detached && !explicit && !out.Registered {
		out.Message = detachedStatusMessage(out.Message)
	}
	if srv.opts.claudeFollow != nil {
		if result := srv.opts.claudeFollow.summary(); result != "" {
			out.Message += " " + result
		}
	}
	srv.mu.Lock()
	waiting := b.holderWaiting
	srv.mu.Unlock()
	if waiting {
		out.Message = registrationInUseGuidance
	}
	return nil, authVersions(srv.ctx, target, srv.opts.deviceIdentityStatus(claimAuthStatus(b, out)), id.Harness), nil
}

func (srv *ingressServer) tokenAuthStatus(callCtx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, authStatus, error) {
	if upstreamRejected(srv.upstream) {
		return nil, authVersions(srv.ctx, srv.upstream, srv.opts.deviceIdentityStatus(authStatus{Status: authRejected, Message: credentialGuidance(false)})), nil
	}
	if failure := projectTerminalUpstream(srv.upstream); failure != nil {
		return nil, authVersions(srv.ctx, srv.upstream, srv.opts.deviceIdentityStatus(authStatus{Status: authFailed, Message: failure.Error()})), nil
	}
	id, err := srv.identify(callCtx, req)
	if err != nil {
		return nil, authStatus{}, err
	}
	srv.mu.Lock()
	target := srv.upstream
	out := authStatus{Status: authAuthorized, Message: "configured-session-credential: The broker accepted the configured session credential. If registration has not resumed, call attach_session to resume the saved registration."}
	if b := srv.bindings[id.SessionID]; b != nil && b.durable != nil {
		out.Registered, out.Name, out.RegistrationID = b.started && !b.failed, b.durable.name, b.durable.registrationID
		if b.receiveReason != "" {
			out.ReceiveError = b.receiveReason
			out.Message = claudeInboxRecovery
		}
		target = srv.upstreamFor(b)
	}
	b := srv.bindings[id.SessionID]
	knownDetached := b != nil && b.durable != nil && b.durable.detached
	srv.mu.Unlock()
	if !knownDetached && (b == nil || !srv.opts.automaticRegistrationAllowed(b)) {
		out.Status = authUnauthorized
		out.Registered = false
		out.Message = conversationNotRegistered
	}
	if b != nil {
		srv.mu.Lock()
		detached := b.resumeStopReason == "registration-detached" || b.durable.detached
		srv.mu.Unlock()
		if detached && !out.Registered {
			out.Message = detachedStatusMessage(out.Message)
		}
	}
	if b != nil {
		out.Diagnostics = srv.opts.diagnostics(b)
		if b.durable != nil {
			out.HandoverFrom = b.durable.handoverFrom
		}
	}
	if b != nil {
		srv.mu.Lock()
		waiting := b.holderWaiting
		srv.mu.Unlock()
		if waiting {
			out.Message = registrationInUseGuidance
		}
	}
	if failure := srv.opts.projectTerminal(b); failure != nil {
		out.Message = failure.Error()
	}
	return nil, authVersions(srv.ctx, target, claimAuthStatus(b, srv.opts.deviceIdentityStatus(authScopes(callCtx, target, out))), id.Harness), nil
}
