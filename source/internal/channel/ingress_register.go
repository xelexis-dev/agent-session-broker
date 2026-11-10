package channel

import (
	"context"
	"crypto/rand"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"time"
)

func (srv *ingressServer) registerDurable(callCtx context.Context, identity broker.AttachRequest) (broker.Session, error) {
	srv.mu.Lock()
	b := srv.bindings[identity.SessionID]
	if srv.stopping || b == nil {
		srv.mu.Unlock()
		return broker.Session{}, errors.New("ingress client is shutting down or the binding is missing")
	}
	// The handler writes state; shutdown joins it before releasing locks.
	srv.workers.Add(1)
	srv.mu.Unlock()
	defer srv.workers.Done()
	callCtx, cancel := context.WithCancel(callCtx)
	stop := context.AfterFunc(srv.ctx, cancel)
	defer stop()
	defer cancel()
	if a := b.durable.session; a != nil {
		a.mu.Lock()
		if a.state.AutoRecovery != nil && a.state.AutoRecovery.StopReason == "project-deleted" {
			a.state.AutoRecovery = nil
		}
		if auth, ok := srv.opts.deviceAuthorizerClient(); ok {
			auth.projectState.reset(auth.projectSource)
			if a.state.Credential != nil {
				auth.projectState.reset(credentialSource(a.state.Credential.AccessToken))
			}
		}
		a.mu.Unlock()
	}
	resetProjectUpstream(srv.upstream)
	callCtx = context.WithValue(callCtx, manualExplicitAttachKey{}, true)
	if err := srv.opts.prepareConversationHandover(callCtx, b, identity); err != nil {
		return broker.Session{}, err
	}
	if err := srv.opts.beginInitialRegistration(b); err != nil {
		return broker.Session{}, err
	}
	if srv.opts.device != nil {
		a := b.durable.session
		a.mu.Lock()
		recovering := a.recovering
		a.mu.Unlock()
		if recovering {
			return broker.Session{}, errExplicitBindingStale
		}
		// An explicit attach_session is the user's request: it may start
		// a new authorization after a denial, expiry or revocation.
		switch srv.deviceStatus(b) {
		case authAuthorized:
			if err := srv.opts.verifyStoredScopes(callCtx, b.durable.session); err != nil {
				return broker.Session{}, err
			}
			if err := srv.opts.finishManualHandover(callCtx, b, identity); err != nil {
				return broker.Session{}, err
			}
			target := srv.ensureUpstream(b)
			if target == nil {
				return broker.Session{}, errors.New("ingress client is shutting down")
			}
			identity.Name = srv.credentialName(b)
			srv.mu.Lock()
			savedRegistrationID, started := b.durable.registrationID, b.started && !b.failed
			srv.mu.Unlock()
			if savedRegistrationID != "" {
				if started {
					var current broker.Session
					err := call(callCtx, target, "get_session", map[string]string{"name": broker.SelectorRegistration + savedRegistrationID}, &current)
					return current, err
				}
				if resumed, ok := resumeDurable(callCtx, target, b, srv.opts, identity.ChannelID); ok {
					return resumed, nil
				}
				if callCtx.Err() != nil {
					return broker.Session{}, callCtx.Err()
				}
				if upstreamRejected(target) {
					return broker.Session{}, srv.opts.explicitCredentialRejection(b.durable.session)
				}
			}
			session, err := registerProjectSafe(callCtx, target, b, srv.opts, identity)
			if err != nil && upstreamRejected(target) {
				return broker.Session{}, srv.opts.explicitCredentialRejection(b.durable.session)
			}
			return session, err
		case authPending:
			srv.opts.updateApprovalNickname(b.durable.session, identity.Name)
			return broker.Session{}, errors.New(b.durable.session.pendingText())
		default:
			if err := srv.beginAuthorization(callCtx, b, identity); err != nil && !errors.Is(err, errAuthorizationPending) {
				return broker.Session{}, err
			}
			if srv.deviceStatus(b) == authAuthorized {
				identity.Name = srv.credentialName(b)
				target := srv.ensureUpstream(b)
				if target == nil {
					return broker.Session{}, errAutomaticStopped
				}
				return registerProjectSafe(callCtx, target, b, srv.opts, identity)
			}
			return broker.Session{}, errors.New(b.durable.session.pendingText())
		}
	}
	return registerProjectSafe(callCtx, srv.upstream, b, srv.opts, identity)
}

func (srv *ingressServer) attached(id broker.AttachRequest, session broker.Session) error {
	var retirementDiagnostic func(string)
	srv.mu.Lock()
	b := srv.bindings[id.SessionID]
	if b == nil || b.identity.ChannelID != id.ChannelID || session.ID != id.SessionID || (b.durable == nil && session.Name != id.Name) {
		srv.mu.Unlock()
		return errors.New("ingress attachment identity mismatch")
	}
	if b.failed || (b.started && b.generation != session.Generation) {
		b.failed = true
		if b.cancel != nil {
			b.cancel()
		}
		registrationID := ""
		if b.durable != nil {
			registrationID = b.durable.registrationID
		}
		target := srv.upstreamFor(b)
		srv.mu.Unlock()
		reportIngressFailure(target, id, registrationID)
		return errors.New("ingress registration was retired; retry registration after the receiver stops")
	}
	announce := false
	if !b.started && srv.stopping {
		srv.mu.Unlock()
		return errors.New("ingress client is shutting down")
	}
	if !b.started {
		b.ingressReady = session.AutomaticDeliveryReady
		b.ingressReadyChannel, b.ingressReadyGeneration = id.ChannelID, session.Generation
	}
	if !b.started {
		srv.workers.Add(1)
		b.identity = id
		if b.durable != nil {
			// The broker may have assigned a UUID nickname; receive on it.
			b.identity.Name = session.Name
			announce = srv.opts.bound != nil
		}
		b.generation = session.Generation
		b.started = true
		if b.durable != nil && b.durable.session != nil {
			a := b.durable.session
			a.mu.Lock()
			a.diagnosticRegistrationID = b.durable.registrationID
			a.diagnosticGeneration = b.generation
			a.mu.Unlock()
		}

		var receiverCtx context.Context
		receiverCtx, b.cancel = context.WithCancel(srv.ctx)
		b.done = make(chan struct{})
		bound := b.identity
		target := srv.upstreamFor(b)
		credential := b.upstreamCredential
		router := b.router
		if router != nil {
			if router.bindRegistration(session.RegistrationID, session.Epoch) {
				retirementDiagnostic = router.steer.diagnose
			}
			router.startReceiver()
		}
		if b.claimThread != "" {
			srv.workers.Add(1)
			go func() {
				defer srv.workers.Done()
				srv.opts.watchCodexYield(srv.ctx, b, &srv.attachMu, &srv.mu, srv.bindings, srv.upstreamFor)
			}()
		}
		go func() {
			defer srv.workers.Done()
			if b.durable != nil {
				runDurableReceiver(receiverCtx, target, b, srv.opts, b.sink)
			} else {
				receiveIngress(receiverCtx, target, bound, b.sink)
			}
			if router != nil {
				router.close()
			}
			if srv.opts.device != nil && upstreamRejected(target) {
				// Preserve the last admission mode before retiring this receiver.
				srv.opts.rejectCredentialIfCurrent(b.durable.session, credential)
			}
			srv.mu.Lock()
			b.failed = true
			close(b.done)
			message := b.followMessage
			srv.mu.Unlock()
			if message != nil && srv.opts.claudeFollow != nil {
				srv.opts.claudeFollow.redeliver(srv.ctx, bound, *message)
			}
			if srv.opts.device != nil && upstreamRejected(target) {
				srv.startRecovery(b)
			}
		}()
	}
	if b.manualSource != "" && srv.opts.claudeFollow != nil {
		source := b.manualSource
		b.manualSource = ""
		srv.workers.Add(1)
		go func() { defer srv.workers.Done(); srv.opts.claudeFollow.publishManualHandover(srv.ctx, source, id) }()
	}
	srv.mu.Unlock()
	if retirementDiagnostic != nil {
		retirementDiagnostic("steer-uncertain-retired")
	}
	if announce {
		srv.opts.bound(session)
	}
	return nil
}

// Commit ownership first, then prepare native ingress and activate the
// same lease before returning it or starting a blocking receiver.
func (srv *ingressServer) finishRegistration(fn registerFunc) registerFunc {
	if fn == nil {
		return nil
	}
	return func(callCtx context.Context, id broker.AttachRequest) (broker.Session, error) {
		srv.mu.Lock()
		if srv.stopping || srv.ctx.Err() != nil {
			srv.mu.Unlock()
			return broker.Session{}, errAutomaticStopped
		}
		srv.workers.Add(1)
		srv.mu.Unlock()
		defer srv.workers.Done()
		callCtx, cancel := context.WithCancel(callCtx)
		stop := context.AfterFunc(srv.ctx, cancel)
		defer stop()
		defer cancel()
		srv.mu.Lock()
		binding := srv.bindings[id.SessionID]
		srv.mu.Unlock()
		var attemptAuth *sessionAuth
		if binding != nil && binding.durable != nil {
			attemptAuth = binding.durable.session
		}
		if attemptAuth != nil {
			attemptAuth.mu.Lock()
			attemptAuth.registrationAttempts++
			attemptAuth.mu.Unlock()
			defer func() { attemptAuth.mu.Lock(); attemptAuth.registrationAttempts--; attemptAuth.mu.Unlock() }()
		}
		session, err := fn(callCtx, id)
		if err != nil {
			return session, err
		}
		srv.mu.Lock()
		b := srv.bindings[id.SessionID]
		deferred := b != nil && b.nativeDeferred && !b.started
		prepared := b != nil && b.sinkPrepared
		srv.mu.Unlock()
		if !deferred {
			return session, nil
		}
		if !prepared {
			sink, router, sinkErr := srv.factory(id)
			srv.mu.Lock()
			if sinkErr == nil && srv.opts.claudeFollow != nil {
				sink = srv.opts.claudeFollow.wrapSink(b, id, sink)
			}
			b.sink, b.router, b.sinkPrepared = sink, router, true
			b.receiveUnavailable = sinkErr != nil
			if sinkErr != nil {
				b.receiveReason = "reconnect-required"
				b.receiveGate = ""
			}
			srv.mu.Unlock()
		}
		srv.mu.Lock()
		unavailable := b.receiveUnavailable
		target := srv.upstreamFor(b)
		srv.mu.Unlock()
		if unavailable || srv.opts.ingressPending() || receiveDenied(target) {
			return session, nil
		}
		var activated broker.RegisterResult
		err = call(callCtx, target, "renew_lease", broker.RenewRequest{RegistrationID: session.RegistrationID, ChannelID: id.ChannelID}, &activated)
		if err != nil {
			return broker.Session{}, err
		}
		if activated.Code != "" || activated.Session.Generation != session.Generation || activated.Session.RegistrationID != session.RegistrationID || !activated.Session.AutomaticDeliveryReady {
			return broker.Session{}, errors.New("native ingress lease activation failed; retry registration shortly")
		}
		return activated.Session, nil
	}
}

func (srv *ingressServer) prepareExplicitAttach(callCtx context.Context, req *mcp.CallToolRequest) (func(context.Context) error, func(), error) {
	if srv.opts.device == nil {
		return nil, nil, nil
	}
	_, parent := srv.opts.device.authorizer.(directAuthorizer)
	id, err := srv.observeIdentity(callCtx, req)
	if err != nil {
		return nil, nil, err
	}
	srv.mu.Lock()
	err = srv.validateIdentity(id)
	b := srv.bindings[id.SessionID]
	srv.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	if b == nil {
		id, err = srv.prepare(callCtx, req)
		if err != nil {
			return nil, nil, err
		}
		srv.mu.Lock()
		b = srv.bindings[id.SessionID]
		srv.mu.Unlock()
	}
	if b == nil || b.durable == nil {
		return nil, nil, nil
	}
	a := b.durable.session
	if a != nil {
		a.mu.Lock()
		a.explicitAttaching++
		a.mu.Unlock()
	}
	release := func() {
		if a != nil {
			a.mu.Lock()
			a.explicitAttaching--
			a.mu.Unlock()
		}
	}
	a.mu.Lock()
	automaticAdmission := a.autoEligibleLocked()
	a.mu.Unlock()
	if !parent || !automaticAdmission {
		return nil, release, nil
	}
	reset := func(callCtx context.Context) error {
		waitCtx, cancel := context.WithTimeout(callCtx, 2*srv.opts.timeout)
		defer cancel()
		if a != nil {
			a.mu.Lock()
			recovering, done, stop := a.recovering, a.recoveryDone, a.recoveryCancel
			a.mu.Unlock()
			if recovering {
				if stop != nil {
					stop()
				}
				select {
				case <-done:
				case <-waitCtx.Done():
					return errors.New("automatic recovery did not stop in time; retry attach_session")
				}
			}
		}
		srv.mu.Lock()
		if srv.stopping || srv.bindings[id.SessionID] != b {
			srv.mu.Unlock()
			return errExplicitBindingStale
		}
		failed, started := b.failed, b.started
		registration := b.durable.registrationID
		target := srv.upstreamFor(b)
		credential := b.upstreamCredential
		srv.mu.Unlock()
		rejected := false
		if registration != "" && target != nil && (failed || started) {
			proofCtx, stop := context.WithTimeout(waitCtx, srv.opts.timeout)
			proofErr := explicitRegistrationProof(proofCtx, target, registration)
			stop()
			rejected = upstreamRejected(target)
			if proofErr != nil && !rejected && !errors.Is(proofErr, errExplicitBindingStale) {
				return proofErr
			}
			failed = failed || proofErr != nil
		}
		if !failed {
			return nil
		}
		srv.mu.Lock()
		stop, done := b.cancel, b.done
		srv.mu.Unlock()
		if stop != nil {
			stop()
		}
		if done != nil {
			select {
			case <-done:
			case <-waitCtx.Done():
				return waitCtx.Err()
			}
		}
		if rejected && a != nil {
			srv.opts.rejectCredentialIfCurrent(a, credential)
		}
		// The barrier gets waitCtx because reset decides on waitCtx; parent cancellation reaches it later.
		if srv.opts.explicitAttachBarrier != nil {
			srv.opts.explicitAttachBarrier(waitCtx, "before-reset")
		}
		if err := waitCtx.Err(); err != nil {
			return err
		}
		b.registerMu.Lock()
		defer b.registerMu.Unlock()
		srv.mu.Lock()
		defer srv.mu.Unlock()
		if err := waitCtx.Err(); err != nil {
			return err
		}
		if srv.stopping || srv.bindings[id.SessionID] != b {
			return errExplicitBindingStale
		}
		if b.retired && !b.selfStopped {
			return errExplicitBindingStale
		}
		if b.selfStopped {
			b.retired, b.selfStopped = false, false
		}
		b.started, b.failed = false, false
		b.identity.ChannelID = rand.Text()
		if b.pendingResume.RequestID != "" {
			b.identity.ChannelID = b.pendingResume.ChannelID
		}
		if b.durable.registrationID == "" {
			b.pendingResume = broker.ResumeRequest{}
		}
		b.messageAck = nil
		if b.router != nil {
			b.router.nextGeneration()
		}
		if srv.opts.explicitAttachBarrier != nil {
			srv.opts.explicitAttachBarrier(waitCtx, "after-reset")
		}
		return nil
	}
	return reset, release, nil
}

func (srv *ingressServer) registerAutomatic(callCtx context.Context, identity broker.AttachRequest) (broker.Session, error) {
	srv.mu.Lock()
	candidate := srv.bindings[identity.SessionID]
	srv.mu.Unlock()
	if failure := projectTerminalUpstream(srv.upstream); failure != nil {
		if candidate != nil && candidate.durable != nil {
			rememberProjectRefusal(candidate, srv.upstream)
		}
		return broker.Session{}, failure
	}
	if candidate == nil || !srv.opts.automaticRegistrationAllowed(candidate) {
		return broker.Session{}, errAutomaticStopped
	}
	srv.mu.Lock()
	claimUnconfirmed := candidate.claimThread != "" && !candidate.claimConfirmed
	claimWithoutSaved := claimUnconfirmed && candidate.durable.registrationID == ""
	srv.mu.Unlock()
	if claimWithoutSaved || (claimUnconfirmed && srv.opts.device != nil && srv.deviceStatus(candidate) != authAuthorized) {
		return broker.Session{}, errAutomaticStopped
	}
	if failure := srv.opts.projectTerminal(candidate); failure != nil {
		return broker.Session{}, failure
	}
	if srv.opts.claudeFollow != nil && callCtx.Value(followIdentityKey{}) == nil && srv.opts.claudeFollow.completionRetryBlocked(identity.SessionID, candidate) {
		return broker.Session{}, errAutomaticStopped
	}
	srv.mu.Lock()
	returning := candidate.durable.handedOver
	srv.mu.Unlock()
	if returning {
		if srv.opts.device == nil || identity.Harness != "claude" {
			return broker.Session{}, errAutomaticStopped
		}
		a := candidate.durable.session
		a.mu.Lock()
		auto := a.state.LastAdmissionMode == "auto"
		a.mu.Unlock()
		if !auto {
			srv.opts.diagnose(candidate, "handover-skipped", "approval-required", "")
			return broker.Session{}, errAutomaticStopped
		}
		if err := srv.opts.prepareConversationHandover(callCtx, candidate, identity); err != nil {
			return broker.Session{}, errAutomaticStopped
		}
	}
	srv.mu.Lock()
	b := srv.bindings[identity.SessionID]
	if srv.opts.device != nil && b != nil && b.durable != nil {
		a := b.durable.session
		a.mu.Lock()
		auto := a.recoveryEligibleLocked(srv.opts)
		rejected := auto && a.state.Credential == nil && a.status != authAuthorized && a.status != authPending
		blocked := a.automaticStoppedLocked() || a.recovering || (a.state.AutoRecovery != nil && a.state.AutoRecovery.StopReason != "")
		a.mu.Unlock()
		if rejected || blocked {
			srv.mu.Unlock()
			if rejected {
				srv.startRecovery(b)
			}
			return broker.Session{}, errAutomaticStopped
		}
	}
	if b == nil || b.started || b.failed {
		srv.mu.Unlock()
		return broker.Session{}, errAutomaticStopped
	}
	saved := b.durable.registrationID != ""
	srv.mu.Unlock()
	if srv.opts.device != nil {
		switch srv.deviceStatus(b) {
		case authAuthorized:
			if err := srv.opts.verifyStoredScopes(callCtx, b.durable.session); err != nil {
				// Temporary public capability discovery failure is not revocation
				// or an unsupported broker. Preserve the saved authorization and let
				// the startup worker retry with its existing cancelable backoff.
				var policyTransient *httpPolicyTransientError
				if errors.Is(err, errDirectFailed) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &policyTransient) {
					return broker.Session{}, err
				}
				b.durable.session.mu.Lock()
				b.durable.session.status = authFailed
				b.durable.session.message = err.Error()
				b.durable.session.mu.Unlock()
				return broker.Session{}, errAutomaticStopped
			}
			if err := srv.opts.finishManualHandover(callCtx, b, identity); err != nil {
				var refusal broker.HandoverRefusal
				if errors.As(err, &refusal) {
					return broker.Session{}, errAutomaticStopped
				}
				return broker.Session{}, err
			}
			srv.mu.Lock()
			saved = b.durable.registrationID != ""
			srv.mu.Unlock()
			target := srv.ensureUpstream(b)
			if target == nil {
				return broker.Session{}, errAutomaticStopped
			}
			identity.Name = srv.credentialName(b)
			if saved {
				result, ok := resumeDurable(callCtx, target, b, srv.opts, identity.ChannelID)
				if ok {
					return result, nil
				}
				if callCtx.Err() != nil {
					return broker.Session{}, callCtx.Err()
				}
				if upstreamRejected(target) {
					srv.opts.rejectCredential(b.durable.session)
					srv.startRecovery(b)
				}
				return broker.Session{}, errAutomaticStopped
			}
			registerCtx, cancel := context.WithTimeout(callCtx, 2*srv.opts.timeout)
			defer cancel()
			session, err := registerDurableWithPolicy(registerCtx, target, b, srv.opts, identity, false)
			if err != nil && upstreamRejected(target) {
				srv.opts.rejectCredential(b.durable.session)
				srv.startRecovery(b)
				return broker.Session{}, errAutomaticStopped
			}
			if errors.Is(err, errInsufficientScope) {
				return broker.Session{}, errAutomaticStopped
			}
			return session, err
		case authPending:
			srv.startPolling(b)
			return broker.Session{}, errAuthorizationPending
		case authUnauthorized, authFailed:
			if err := srv.beginAuthorization(callCtx, b, identity); err != nil {
				return broker.Session{}, err
			}
			identity.Name = srv.credentialName(b)
			target := srv.ensureUpstream(b)
			if target == nil {
				return broker.Session{}, errAutomaticStopped
			}
			if saved {
				result, ok := resumeDurable(callCtx, target, b, srv.opts, identity.ChannelID)
				if !ok {
					return broker.Session{}, errAutomaticStopped
				}
				return result, nil
			}
			return registerDurableWithPolicy(callCtx, target, b, srv.opts, identity, false)
		default:
			// Denied, expired, rejected or failed: the client never requests
			// a new code on its own (RFC 8628 §3.1).
			return broker.Session{}, errAutomaticStopped
		}
	}
	if saved {
		result, ok := resumeDurable(callCtx, srv.upstream, b, srv.opts, identity.ChannelID)
		if !ok {
			if callCtx.Err() != nil {
				return broker.Session{}, callCtx.Err()
			}
			return broker.Session{}, errAutomaticStopped
		}
		return result, nil
	}
	registerCtx, cancel := context.WithTimeout(callCtx, 2*srv.opts.timeout)
	defer cancel()
	session, err := registerDurableWithPolicy(registerCtx, srv.upstream, b, srv.opts, identity, false)
	if errors.Is(err, errInsufficientScope) {
		return broker.Session{}, errAutomaticStopped
	}
	return session, err
}

// Project-token mode: each conversation authorizes itself, then talks to
// the broker through its own upstream. The approval poller uses autoAttach
// to register a newly authorized conversation.
func (srv *ingressServer) autoAttach(callCtx context.Context, req *mcp.CallToolRequest) error {
	if srv.opts.identityFailure != nil {
		return srv.opts.identityFailure
	}
	if state := upstreamCompatibility(srv.currentUpstream(callCtx, req)); !state.Compatible {
		return &compatibilityError{state.Message}
	}

	srv.mu.Lock()
	if srv.stopping || srv.ctx.Err() != nil {
		srv.mu.Unlock()
		return errAutomaticStopped
	}
	srv.workers.Add(1)
	srv.mu.Unlock()
	defer srv.workers.Done()
	callCtx, cancel := context.WithCancel(callCtx)
	stop := context.AfterFunc(srv.ctx, cancel)
	defer stop()
	defer cancel()
	if callCtx.Value(followStartupKey{}) == true {
		// The follow serializer is already held. Ordinary attach owns
		// attachMu before identity observation; do not invert those locks.
		id, err := srv.prepare(callCtx, req)
		if err != nil {
			return err
		}
		session, err := srv.autoRegister(callCtx, id)
		if err != nil {
			return err
		}
		return srv.attached(id, session)
	}
	_, err := srv.attach(callCtx, req, "", srv.autoRegister)
	return err
}

func (srv *ingressServer) continueInitial(b *ingressBinding) {
	if !srv.opts.initialRegistrationPending(b) {
		return
	}
	srv.mu.Lock()
	if srv.stopping || srv.ctx.Err() != nil || b.initialContinuing {
		srv.mu.Unlock()
		return
	}
	b.initialContinuing = true
	srv.workers.Add(1)
	sid := b.identity.SessionID
	srv.mu.Unlock()
	go func() {
		defer srv.workers.Done()
		defer func() { srv.mu.Lock(); b.initialContinuing = false; srv.mu.Unlock() }()
		for attempt := 0; srv.ctx.Err() == nil; attempt++ {
			select {
			case <-srv.ctx.Done():
				return
			case <-time.After(srv.opts.backoff(attempt)):
			}
			err := srv.autoAttach(srv.ctx, deviceRequest(sid))
			if errors.Is(err, errAuthorizationPending) && b.identity.Harness == "claude" {
				continue
			}
			if err == nil || terminalProjectError(err) || errors.Is(err, errAuthorizationPending) || errors.Is(err, errAutomaticStopped) || isCompatibilityError(err) {
				return
			}
		}
	}()
}

func (srv *ingressServer) automatic() {
	if srv.opts.identityFailure != nil {
		return
	}
	srv.automaticOnce.Do(func() {
		srv.mu.Lock()
		if srv.stopping || srv.ctx.Err() != nil {
			srv.mu.Unlock()
			return
		}
		srv.workers.Add(1)
		srv.mu.Unlock()
		go func() {
			defer srv.workers.Done()
			for attempt := 0; srv.ctx.Err() == nil; attempt++ {
				// resumeDurable owns its per-call deadlines and must retain the
				// same request through a slow lost response until process exit.
				err := srv.autoAttach(srv.ctx, nil)
				pendingClaude := false
				if srv.opts.device != nil && errors.Is(err, errAuthorizationPending) {
					srv.mu.Lock()
					for _, b := range srv.bindings {
						pendingClaude = pendingClaude || b.identity.Harness == "claude"
					}
					srv.mu.Unlock()
				}
				srv.mu.Lock()
				startupClaude := false
				for _, b := range srv.bindings {
					startupClaude = startupClaude || (b.identity.Harness == "claude" && !b.started)
				}
				srv.mu.Unlock()
				if errors.Is(err, errAutomaticStopped) && startupClaude && attempt < 5 {
					select {
					case <-srv.ctx.Done():
						return
					case <-time.After(srv.opts.backoff(0)):
					}
					continue
				}
				if err == nil || terminalProjectError(err) || isCompatibilityError(err) || errors.Is(err, errAutomaticStopped) || (errors.Is(err, errAuthorizationPending) && !pendingClaude) {
					return
				}
				// Startup identity may appear shortly after the MCP handshake.
				// Stop probing an unavailable parent after six attempts; a later
				// ordinary tool call can still supply its exact thread metadata.
				srv.mu.Lock()
				unbound := len(srv.bindings) == 0
				srv.mu.Unlock()
				if unbound && attempt >= 5 {
					return
				}
				delay := srv.opts.backoff(attempt)
				if pendingClaude {
					// Identity observation is independent of OAuth polling. Keep
					// checking the parent while its startup code is pending.
					delay = srv.opts.backoff(0)
				}
				select {
				case <-srv.ctx.Done():
					return
				case <-time.After(delay):
				}
			}
		}()
	})
}
