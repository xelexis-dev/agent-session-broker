package channel

import (
	"context"
	"crypto/rand"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func (srv *ingressServer) startRecovery(b *ingressBinding) {
	if b != nil {
		srv.mu.Lock()
		retired := b.retired || (b.durable != nil && b.durable.handedOver) || (b.claimThread != "" && !b.claimConfirmed)
		srv.mu.Unlock()
		if retired {
			return
		}
	}
	if srv.opts.device == nil || b == nil || b.durable == nil {
		return
	}
	a0 := b.durable.session
	a0.mu.Lock()
	stopped := a0.automaticStoppedLocked()
	detached := a0.state.Registration.Detached
	a0.mu.Unlock()
	if stopped {
		if detached {
			srv.opts.stopAutoRecovery(a0, "registration-detached")
		}
		return
	}
	if !srv.opts.automaticRegistrationAllowed(b) {
		return
	}
	srv.mu.Lock()
	a := b.durable.session
	a.mu.Lock()
	if srv.stopping || srv.ctx.Err() != nil || b.retired || b.durable.handedOver || !a.recoveryEligibleLocked(srv.opts) || a.automaticStoppedLocked() || a.state.Credential != nil || a.recovering || a.explicitAttaching > 0 || (a.state.AutoRecovery != nil && a.state.AutoRecovery.StopReason != "" && !srv.opts.admissionChangedLocked(a)) {
		a.mu.Unlock()
		srv.mu.Unlock()
		return
	}
	recoveryCtx, cancelRecovery := context.WithCancel(srv.ctx)
	recoveryDone := make(chan struct{})
	a.recovering, a.recoveryCancel, a.recoveryDone = true, cancelRecovery, recoveryDone
	b.failed = true
	if a.state.AutoRecovery == nil {
		a.state.AutoRecovery = &clientstate.AutoRecovery{}
	}
	done := b.done
	if b.cancel != nil {
		b.cancel()
	}
	srv.workers.Add(1)
	a.mu.Unlock()
	srv.mu.Unlock()
	go func() {
		defer cancelRecovery()
		defer srv.workers.Done()
		defer func() {
			srv.mu.Lock()
			a.mu.Lock()
			a.recovering = false
			a.recoveryCancel = nil
			close(recoveryDone)
			again := a.explicitAttaching == 0 && !a.automaticStoppedLocked() && !srv.stopping && recoveryCtx.Err() == nil && b.failed && a.state.AutoRecovery == nil && upstreamRejected(b.upstream)
			pending := a.explicitAttaching == 0 && !a.automaticStoppedLocked() && !srv.stopping && recoveryCtx.Err() == nil && a.status == authPending && (a.state.AutoRecovery == nil || a.state.AutoRecovery.StopReason == "")
			a.mu.Unlock()
			srv.mu.Unlock()
			if again {
				srv.startRecovery(b)
			}
			if pending {
				srv.startPolling(b)
			}
		}()
		// A new upstream must never overlap the old receiver or its heartbeat.
		if done != nil {
			select {
			case <-recoveryCtx.Done():
				return
			case <-done:
			}
		}
		a.exchangeMu.Lock()
		defer a.exchangeMu.Unlock()
		a.mu.Lock()
		explicit := a.explicitAttaching > 0
		a.mu.Unlock()
		if recoveryCtx.Err() != nil || explicit {
			return
		}
		a.mu.Lock()
		pollCancel, pollDone := a.pollCancel, a.pollDone
		a.mu.Unlock()
		if pollCancel != nil {
			pollCancel()
			if pollDone != nil {
				select {
				case <-recoveryCtx.Done():
					return
				case <-pollDone:
				}
			}
		}
		b.registerMu.Lock()
		srv.mu.Lock()
		if srv.stopping || srv.bindings[b.identity.SessionID] != b {
			srv.mu.Unlock()
			b.registerMu.Unlock()
			return
		}
		b.started, b.failed = false, false
		id := b.identity
		id.ChannelID = rand.Text()
		if b.pendingResume.RequestID != "" {
			id.ChannelID = b.pendingResume.ChannelID
		}
		b.identity.ChannelID = id.ChannelID
		srv.mu.Unlock()
		b.registerMu.Unlock()
		if err := srv.opts.transitionAdmission(a); err != nil {
			srv.opts.stopAutoRecovery(a, "local-state-write-failed")
			return
		}
		if !srv.opts.reauthorize(recoveryCtx, a, id) {
			return
		}
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
		target := srv.ensureUpstream(b)
		if target == nil {
			srv.opts.stopAutoRecovery(a, "local-connection-unavailable")
			return
		}
		recoverRegistration := srv.finishRegistration(func(recoverCtx context.Context, _ broker.AttachRequest) (broker.Session, error) {
			srv.mu.Lock()
			saved := b.durable.registrationID != ""
			srv.mu.Unlock()
			if !saved {
				if !srv.opts.automaticRegistrationAllowed(b) {
					return broker.Session{}, errAutomaticStopped
				}
				return registerDurableWithPolicy(recoverCtx, target, b, srv.opts, id, false)
			}
			result, ok := resumeDurable(recoverCtx, target, b, srv.opts, id.ChannelID)
			if !ok {
				return broker.Session{}, errRecoveryRegistration
			}
			return result, nil
		})
		result, err := recoverRegistration(recoveryCtx, id)
		if recoveryCtx.Err() != nil {
			return
		}
		if err == nil {
			err = srv.attached(id, result)
		}
		if err != nil {
			srv.mu.Lock()
			reason := b.resumeStopReason
			srv.mu.Unlock()
			if reason == "" {
				reason = "registration-invalid"
			}
			srv.opts.stopAutoRecovery(a, reason)
			return
		}
		a.mu.Lock()
		a.state.AutoRecovery = nil
		a.mu.Unlock()
		_ = srv.opts.persistSession(a)
	}()
}
