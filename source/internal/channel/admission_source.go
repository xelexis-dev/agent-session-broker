package channel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
)

// This fingerprint is state-only. Never include it in diagnostics or guidance.
func (c *deviceAuthClient) admissionFingerprint() string {
	sum := sha256.Sum256([]byte(c.projectToken))
	return hex.EncodeToString(sum[:])
}
func (o durableOptions) admissionFingerprint() string {
	if source, ok := o.device.authorizer.(interface{ admissionFingerprint() string }); ok {
		return source.admissionFingerprint()
	}
	return ""
}
func (o durableOptions) admissionChangedLocked(a *sessionAuth) bool {
	current := o.admissionFingerprint()
	return a.state.Credential == nil && a.state.AdmissionFingerprint != "" && current != "" && a.state.AdmissionFingerprint != current
}
func (a *sessionAuth) recoveryEligibleLocked(o durableOptions) bool {
	if !registrationPermitted(a.state.Registration) || a.state.Direct != nil && a.state.Direct.Reattach {
		return false
	}
	if o.admissionChangedLocked(a) {
		return true
	}
	if a.autoEligibleLocked() || a.state.AutoRecovery != nil {
		return true
	}
	// Unknown source is not evidence of auto. A one-time direct-only migration
	// consumes this token's budget when its fingerprint is first committed.
	return a.state.Credential == nil && a.state.AdmissionFingerprint == "" && o.admissionFingerprint() != "" && a.state.Device != nil && a.state.Device.Status != clientstate.DevicePending && a.state.Device.Status != clientstate.DeviceRequested
}

// Called with exchange ownership (or during initial load, before publishing the
// binding). The old poller is joined by the caller. Publish one saved snapshot
// containing the new source, cleared authorization, and the new request ID.
func (o durableOptions) transitionAdmission(a *sessionAuth) error {
	current := o.admissionFingerprint()
	a.mu.Lock()
	if !registrationPermitted(a.state.Registration) || current == "" || a.state.Credential != nil {
		a.mu.Unlock()
		return nil
	}
	previous := a.state.AdmissionFingerprint
	if previous == current {
		a.mu.Unlock()
		return nil
	}
	kind := ""
	if previous != "" {
		kind = "token-changed"
	} else {
		d := a.state.Device
		if (d != nil && (d.Status == clientstate.DevicePending || d.Status == clientstate.DeviceRequested)) || (a.state.Direct != nil && a.state.Direct.Status == "requested") {
			a.mu.Unlock()
			return nil
		}
		if d != nil || a.state.LastAdmissionMode == "auto" || (a.state.Direct != nil && a.state.Direct.Status != "requested") {
			kind = "legacy-migration"
		}
	}
	candidate := a.state
	restricted := candidate.RestrictedAdmission || (candidate.Direct != nil && candidate.Direct.Restricted)
	candidate.AdmissionFingerprint = current
	candidate.RestrictedAdmission = restricted
	candidate.Device = nil
	candidate.Direct = &clientstate.DirectAuthorization{RequestedNickname: o.device.nickname, RequestID: rand.Text(), CreatedAt: o.now(), Status: "requested", Restricted: restricted}
	candidate.AutoRecovery = nil
	if kind != "" {
		candidate.AutoRecovery = &clientstate.AutoRecovery{Kind: kind}
	}
	if kind == "token-changed" {
		candidate.LastAdmissionMode = ""
	}
	a.mu.Unlock()
	if err := o.persistSessionState(a.key, candidate); err != nil {
		return errors.New("admission state could not be saved; no authorization request was sent")
	}
	a.mu.Lock()
	a.state = candidate
	a.deriveStatus(o.now())
	a.mu.Unlock()
	if kind != "" && o.state != nil {
		_ = o.state.RecordDiagnostic(a.key, clientstate.DiagnosticEvent{At: o.now(), SessionID: candidate.SessionID, RegistrationID: candidate.Registration.RegistrationID, Event: kind})
	}
	return nil
}

func (o durableOptions) checkScopedAdmission(ctx context.Context, a *sessionAuth) error {
	a.mu.Lock()
	needed := a.state.RestrictedAdmission || (a.state.Direct != nil && a.state.Direct.Restricted)
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
	return nil
}

// Called under exchangeMu by explicit attach; automatic retries keep their replay ID.
func (o durableOptions) resetExplicitAdmission(a *sessionAuth) error {
	a.mu.Lock()
	candidate := a.state
	if a.explicitAttaching == 0 || candidate.Credential != nil || (candidate.AutoRecovery == nil && (candidate.Direct == nil || candidate.Direct.Status != "requested")) {
		a.mu.Unlock()
		return nil
	}
	preserveN1 := candidate.AutoRecovery == nil && candidate.Registration.RegistrationID != ""
	candidate.AutoRecovery = nil
	expiredReattach := candidate.Direct != nil && candidate.Direct.Reattach && o.now().Sub(candidate.Direct.CreatedAt) >= 10*time.Minute
	if expiredReattach {
		candidate.Direct = nil
	}
	if candidate.Direct != nil {
		direct := *candidate.Direct
		if !((direct.Reattach || preserveN1) && direct.Status == "requested" && o.now().Sub(direct.CreatedAt) < 10*time.Minute) {
			direct.RequestID, direct.CreatedAt, direct.Status = rand.Text(), o.now(), "requested"
			direct.Reattach = false
		}
		candidate.Direct = &direct
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
	if expiredReattach {
		return errDirectPendingExpired
	}
	return nil
}
