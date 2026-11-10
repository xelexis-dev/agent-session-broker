package channel

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// authExchange serializes both discovery and exchange for a binding, without
// blocking auth_status. The binding's OS session lock fences other processes.
var errReattachRefused = errors.New("reattach-refused: The parent token cannot re-admit this conversation. Check the project token or attach a new conversation.")

func (o durableOptions) tryDirect(ctx context.Context, a *sessionAuth, id broker.AttachRequest) (bool, error) {
	return o.tryDirectWithPolicy(ctx, a, id, false)
}

func (o durableOptions) tryDirectWithPolicy(ctx context.Context, a *sessionAuth, id broker.AttachRequest, recovery bool) (bool, error) {
	a.mu.Lock()
	explicit := a.explicitAttaching > 0
	detached := (a.state.Registration.Detached || a.state.Direct != nil && a.state.Direct.Reattach) && !explicit
	a.mu.Unlock()
	if detached {
		return true, errAutomaticStopped
	}
	// Recovery accepts only the validated scoped direct grant below. It never
	// needs discovery (whose unavailability is not evidence of lost support).
	if !recovery {
		if err := o.checkScopedAdmission(ctx, a); err != nil {
			return true, err
		}
	}
	direct, ok := o.device.authorizer.(directAuthorizer)
	if !ok {
		if recovery {
			return true, errDirectUnsupported
		}
		return false, nil
	}
	supported := true
	var err error
	if !recovery {
		supported, err = direct.discover(ctx)
	}
	if err != nil {
		return true, err
	}
	if !supported {
		a.mu.Lock()
		restricted := a.state.Direct != nil && a.state.Direct.Restricted
		a.mu.Unlock()
		if restricted {
			return true, errScopeUpgrade
		}
		return false, nil
	}
	a.mu.Lock()
	candidate := a.state
	if recovery && (candidate.Registration.Secret == "" || (candidate.Registration.RegistrationID == "" && candidate.Registration.InitialAttempt != initialPending)) {
		a.mu.Unlock()
		return true, errRecoveryRegistration
	}
	pending := candidate.Direct
	if pending == nil || pending.Status != "requested" {
		restricted := pending != nil && pending.Restricted
		pending = &clientstate.DirectAuthorization{Restricted: restricted, RequestID: rand.Text(), RequestedNickname: o.device.nickname, CreatedAt: o.now(), Status: "requested"}
	} else if o.now().Sub(pending.CreatedAt) >= 10*time.Minute {
		a.mu.Unlock()
		return true, errDirectPendingExpired
	}
	copied := *pending
	candidate.Direct = &copied
	candidate.Device = nil
	candidate.Credential = nil
	request := directCodeRequest{Recovery: recovery, deviceCodeRequest: deviceCodeRequest{Harness: id.Harness, SessionID: id.SessionID, ClientID: candidate.ClientID, RequestID: pending.RequestID}, Nickname: pending.RequestedNickname, RegistrationID: candidate.Registration.RegistrationID, Secret: candidate.Registration.Secret}
	if copied.Reattach {
		request.RegistrationID = ""
	} else if recovery || candidate.Registration.RegistrationID != "" {
		request.Nickname = ""
		copied.RequestedNickname = ""
	}
	a.mu.Unlock()
	if err := o.persistSessionState(a.key, candidate); err != nil {
		return true, errDirectFailed
	}
	a.mu.Lock()
	liveDirect := *candidate.Direct
	a.state.Direct = &liveDirect
	a.state.Device = nil
	a.mu.Unlock()
	grant, err := o.exchangeAuthHolder(ctx, a, id, direct, request)
	var refusal broker.HandoverRefusal
	if !recovery && explicit && request.RegistrationID != "" && errors.As(err, &refusal) && refusal.Reason == broker.CodeNotFound {
		candidate.Direct = &clientstate.DirectAuthorization{RequestID: rand.Text(), RequestedNickname: candidate.Registration.Name, CreatedAt: o.now(), Status: "requested", Restricted: copied.Restricted, Reattach: true}
		if o.persistSessionState(a.key, candidate) != nil {
			return true, errDirectFailed
		}
		a.mu.Lock()
		saved := *candidate.Direct
		a.state.Direct = &saved
		a.mu.Unlock()
		request.RequestID = candidate.Direct.RequestID
		request.Nickname = candidate.Direct.RequestedNickname
		request.RegistrationID = ""
		grant, err = o.exchangeAuthHolder(ctx, a, id, direct, request)
	}
	if err != nil {
		if candidate.Direct.Reattach {
			if initialPermanent(err) {
				candidate.Direct.Status = "failed"
				candidate.Direct.Reattach = false
				if o.persistSessionState(a.key, candidate) != nil {
					return true, errDirectFailed
				}
				a.mu.Lock()
				saved := *candidate.Direct
				a.state.Direct = &saved
				a.mu.Unlock()
			}
			if initialPermanent(err) {
				return true, errors.Join(errReattachRefused, err)
			}
			return true, err
		}
		if errors.Is(err, errDirectCapacity) && o.state != nil {
			// Keep the existing closed diagnostic reason vocabulary; never copy
			// the remote error body into local diagnostic attachments.
			_ = o.state.RecordDiagnostic(a.key, clientstate.DiagnosticEvent{At: o.now().UTC(), SessionID: id.SessionID, RegistrationID: candidate.Registration.RegistrationID, Event: "reconnect-failed", Reason: "capacity", ErrorClass: "other"})
		}

		if !recovery && errors.Is(err, errInsufficientScope) {
			a.mu.Lock()
			restricted := a.state.Direct.Restricted
			a.mu.Unlock()
			a.mu.Lock()
			changed := a.state.AutoRecovery != nil && a.state.AutoRecovery.Kind == "token-changed"
			a.mu.Unlock()
			if changed && restricted {
				if err := o.checkScopedAdmission(ctx, a); err != nil {
					return true, err
				}
			}
			if !restricted || changed {
				return false, nil
			}
		}
		return true, err
	}
	if candidate.Direct.Reattach {
		candidate.Registration.RegistrationID = ""
		candidate.Registration.Revision = 0
		candidate.Registration.Detached = false
		candidate.Registration.InitialAttempt = initialPending
		candidate.Registration.PendingRequestID = ""
		candidate.Registration.PendingLease = ""
		candidate.Registration.PendingRevision = 0
		candidate.Direct.Reattach = false
	}
	now := o.now()
	candidate.Credential = &clientstate.SessionCredential{AccessToken: grant.AccessToken, TokenType: grant.TokenType, IssuedAt: now, ExpiresAt: now.Add(time.Duration(grant.ExpiresIn) * time.Second), SessionGrantID: grant.SessionGrantID, Nickname: grant.Nickname, Scopes: append([]string{}, grant.Scopes...), AdmissionMode: grant.Mode}
	candidate.LastAdmissionMode = grant.Mode
	candidate.RestrictedAdmission = access.RestrictedScopes(grant.Scopes)
	candidate.Direct.Status = "redeemed"
	candidate.Direct.Restricted = access.RestrictedScopes(grant.Scopes)
	if grant.RegistrationID != "" {
		if candidate.Registration.RegistrationID != grant.RegistrationID {
			return true, errDirectConflict
		}
		candidate.Registration.Revision = grant.Revision
		candidate.Registration.Name = grant.Nickname
	}
	if err := o.persistSessionState(a.key, candidate); err != nil {
		return true, errDirectFailed
	}
	a.mu.Lock()
	a.state = candidate
	a.scopesVerified = true
	a.deriveStatus(now)
	a.mu.Unlock()
	return true, nil
}

// A saved restricted snapshot must not silently gain full rights on an old
// server after downgrade. Gate-off servers can still advertise scoped support.
func (o durableOptions) verifyStoredScopes(ctx context.Context, a *sessionAuth) error {
	a.mu.Lock()
	needed := !a.scopesVerified && a.state.Credential != nil && a.state.Credential.Scopes != nil && access.RestrictedScopes(a.state.Credential.Scopes)
	a.mu.Unlock()
	if !needed {
		return nil
	}
	checker, ok := o.device.authorizer.(interface {
		scoped(context.Context) (bool, error)
	})
	if !ok {
		return errScopeUpgrade
	}
	supported, err := checker.scoped(ctx)
	if err != nil {
		return err
	}
	if !supported {
		return errScopeUpgrade
	}
	a.mu.Lock()
	a.scopesVerified = true
	a.mu.Unlock()
	return nil
}
