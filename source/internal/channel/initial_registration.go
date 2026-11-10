package channel

import (
	"context"
	"errors"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

const (
	initialPending            = "pending"
	initialComplete           = "complete"
	initialStopped            = "stopped"
	conversationNotRegistered = "this conversation is not registered; call attach_session when the user asks to connect"
	registrationInstructions  = "New conversations register with attach_session only when the user asks to connect. Registered conversations resume automatically after restart or reconnect. Do not call attach_session merely because the MCP client started or a diagnostic/tool call failed."
)

func registrationPermitted(r clientstate.Registration) bool {
	return !r.Detached && r.Secret != "" && (r.RegistrationID != "" || r.InitialAttempt == initialPending)
}

// Legacy pending artifacts are compatibility evidence of unfinished work, not
// evidence that a human explicitly requested that historical attempt.
func inferInitialAttempt(s *clientstate.SessionState, now time.Time) {
	r := &s.Registration
	if r.RegistrationID != "" {
		return
	}
	terminal := false
	if d := s.Device; d != nil {
		terminal = d.Status == clientstate.DeviceDenied || d.Status == clientstate.DeviceExpired || d.Status == clientstate.DeviceFailed ||
			(d.Status == clientstate.DevicePending && !now.Before(d.ExpiresAt))
	}
	if terminal {
		r.InitialAttempt = initialStopped
		return
	}
	if r.InitialAttempt != "" {
		return
	}
	if r.PendingRequestID != "" && r.PendingLease != "" {
		r.InitialAttempt = initialPending
		return
	}
	if d := s.Device; d != nil && (d.Status == clientstate.DeviceRequested || d.Status == clientstate.DevicePending || (d.Status == clientstate.DeviceRedeemed && s.Credential != nil)) {
		r.InitialAttempt = initialPending
		return
	}
	if d := s.Direct; d != nil {
		if d.Status == "requested" {
			if now.Sub(d.CreatedAt) < 10*time.Minute {
				r.InitialAttempt = initialPending
			} else {
				r.InitialAttempt = initialStopped
			}
		} else if d.Status == "redeemed" && s.Credential != nil {
			r.InitialAttempt = initialPending
		}
	}
}
func registrationSnapshot(d *durableRegistration, id broker.AttachRequest, project string) clientstate.Registration {
	return clientstate.Registration{ParentPID: d.parentPID, ProcessKey: d.processKey, SavedAt: time.Now().UTC(), HandoverFrom: d.handoverFrom, Detached: d.detached, RegistrationID: d.registrationID, Name: d.name, Secret: d.secret, Revision: d.revision, Project: project, Harness: id.Harness, SessionID: id.SessionID, PendingRequestID: d.pendingRequest, PendingLease: d.pendingLease, PendingRevision: d.pendingRevision, InitialAttempt: d.initialAttempt}
}
func (o durableOptions) automaticRegistrationAllowed(b *ingressBinding) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.durable == nil || b.durable.detached {
		return false
	}
	if a := b.durable.session; a != nil {
		a.mu.Lock()
		defer a.mu.Unlock()
		return registrationPermitted(a.state.Registration) && (a.state.Direct == nil || !a.state.Direct.Reattach)
	}
	return b.durable.secret != "" && (b.durable.registrationID != "" || b.durable.initialAttempt == initialPending)
}

// Called only by the attach_session handler. Save before publishing permission
// so a write failure cannot let the next diagnostic start an admission request.
func (o durableOptions) beginInitialRegistration(b *ingressBinding) error {
	b.registerMu.Lock()
	defer b.registerMu.Unlock()
	b.mu.Lock()
	d := b.durable
	id := b.identity
	copied := *d
	b.mu.Unlock()
	if d.registrationID != "" {
		return nil
	}
	copied.initialAttempt = initialPending
	if a := d.session; a != nil {
		a.mu.Lock()
		candidate := a.state
		if candidate.Registration.InitialAttempt == initialPending {
			a.mu.Unlock()
			return nil
		}
		candidate.Registration = registrationSnapshot(&copied, id, o.project)
		candidate.Device, candidate.Direct, candidate.AutoRecovery = nil, nil, nil
		if candidate.Credential == nil {
			candidate.LastAdmissionMode = ""
		}
		a.mu.Unlock()
		if err := o.persistSessionState(a.key, candidate); err != nil {
			return err
		}
		a.mu.Lock()
		a.state = candidate
		a.message = ""
		a.deriveStatus(o.now())
		a.mu.Unlock()
	} else {
		if d.initialAttempt == initialPending {
			return nil
		}
		if err := o.state.SaveRegistration(d.key, registrationSnapshot(&copied, id, o.project)); err != nil {
			return err
		}
	}
	b.mu.Lock()
	d.initialAttempt = initialPending
	b.mu.Unlock()
	return nil
}
func (o durableOptions) stopInitialRegistration(a *sessionAuth) {
	a.mu.Lock()
	if a.state.Registration.RegistrationID == "" {
		a.state.Registration.InitialAttempt = initialStopped
	}
	a.mu.Unlock()
	_ = o.persistSession(a)
}
func initialPermanent(err error) bool {
	var transient *directTransientError
	return err != nil && !errors.As(err, &transient) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, errAuthorizationPending)
}

func (o durableOptions) stopInitialBinding(b *ingressBinding, id broker.AttachRequest) {
	b.mu.Lock()
	if b.durable.registrationID != "" {
		b.mu.Unlock()
		return
	}
	b.durable.initialAttempt = initialStopped
	saved := *b.durable
	b.mu.Unlock()
	_ = o.save(id, &saved)
}

func (o durableOptions) initialRegistrationPending(b *ingressBinding) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.durable == nil || b.durable.registrationID != "" {
		return false
	}
	if a := b.durable.session; a != nil {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.state.Registration.InitialAttempt == initialPending
	}
	return b.durable.initialAttempt == initialPending
}
