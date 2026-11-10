package channel

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type takeoverContextKey struct{}
type handoverSourceContextKey struct{}

func (o durableOptions) handoverProcessKey(id broker.AttachRequest) string {
	if id.Harness != "claude" {
		return ""
	}
	fn := o.parentProcessKey
	if fn == nil {
		fn = claudeParentProcessKey
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	key, err := fn(ctx, id.PID)
	if err != nil {
		return ""
	}
	return key
}
func handoverError(reason string) error { return broker.HandoverRefusal{Reason: reason} }
func handoverResolutionReason(err error) string {
	if errors.Is(err, clientstate.ErrHandoverDetached) {
		return "detached"
	}
	if errors.Is(err, clientstate.ErrHandoverNotFound) {
		return "not-found"
	}
	return "refused"
}

// Explicit attach may select a saved source. Resume-follow may also invoke
// this with an exact same-process source, without granting fresh admission.
func (o durableOptions) prepareConversationHandover(ctx context.Context, b *ingressBinding, id broker.AttachRequest) error {
	takeover, _ := ctx.Value(takeoverContextKey{}).(string)
	explicit := takeover != ""
	forcedSource, _ := ctx.Value(handoverSourceContextKey{}).(string)
	_, following := ctx.Value(followIdentityKey{}).(broker.AttachRequest)
	explicitAttach, _ := ctx.Value(manualExplicitAttachKey{}).(bool)
	// Follow also sets takeover internally; only a user attach may revive detach.
	allowDetached := (explicit && !following) || (following && explicitAttach)
	b.mu.Lock()
	d := b.durable
	registration, name := d.registrationID, d.name
	returning := d.handedOver
	clientID, key, processKey := d.clientID, d.key, d.processKey
	b.mu.Unlock()
	if explicit && (id.Harness != "claude" || o.device == nil) {
		return handoverError("unsupported-mode")
	}
	if registration != "" && !returning {
		if explicit && takeover != name {
			return handoverError("not-found")
		}
		return nil
	}
	if id.Harness != "claude" {
		return nil
	}
	candidates, err := o.state.HandoverCandidates(o.scope, "claude", clientID)
	if err != nil {
		return err
	}
	var selected *clientstate.SavedConversation
	if returning {
		holder, e := o.state.HandoverHolder(key, o.scope, "claude", clientID, allowDetached)
		if e != nil {
			reason := handoverResolutionReason(e)
			o.diagnose(b, "handover-skipped", reason, "")
			return handoverError(reason)
		}
		selected = &holder
	}
	for i := range candidates {
		candidate := &candidates[i]
		r := candidate.State.Registration
		if candidate.Key == key || (forcedSource != "" && candidate.Key != forcedSource) {
			continue
		}
		match := forcedSource != "" || (explicit && r.Name == takeover)
		if !explicit && forcedSource == "" {
			match = processKey != "" && r.ParentPID == id.PID && r.ProcessKey == processKey
		}
		if match && (forcedSource != "" || (!returning && (selected == nil || r.SavedAt.After(selected.State.Registration.SavedAt)))) {
			selected = candidate
		}
	}
	if selected == nil || (forcedSource != "" && selected.Key != forcedSource) {
		if explicit {
			return handoverError("not-found")
		}
		return nil
	}
	skip := func(reason string) error {
		reason = broker.SafeHandoverReason(reason)
		o.diagnose(b, "handover-skipped", reason, "")
		if explicit || returning || forcedSource != "" {
			return handoverError(reason)
		}
		return nil
	}
	// Saved revisions, rather than marker completeness or SavedAt, decide
	// which owned conversation can prove the registration's current holder.
	holder, resolveErr := o.state.HandoverHolder(selected.Key, o.scope, "claude", clientID, allowDetached)
	if resolveErr != nil {
		reason := handoverResolutionReason(resolveErr)
		o.diagnose(b, "handover-skipped", reason, "")
		return handoverError(reason) // Ambiguity never falls through to fresh admission.
	}
	selected = &holder
	if selected.Key == key {
		b.mu.Lock()
		d.handedOver = false
		b.mu.Unlock()
		return nil
	}
	if o.device == nil {
		return skip("unsupported-mode")
	}
	// Only actual source use refreshes retention; discovery must stay read-only.
	if _, found, err := o.state.LoadSession(selected.Key); err != nil {
		return err
	} else if !found {
		return skip("not-found")
	}
	if selected.State.LastAdmissionMode == "manual" || (selected.State.Credential != nil && selected.State.Credential.AdmissionMode == "manual") {
		return o.prepareManualHandover(ctx, b, id, *selected)
	}
	direct, ok := o.device.authorizer.(directAuthorizer)
	if !ok {
		return skip("unsupported-mode")
	}
	a := d.session
	a.exchangeMu.Lock()
	defer a.exchangeMu.Unlock()
	a.mu.Lock()
	attempt := a.state.Handover
	candidate := a.state
	a.mu.Unlock()
	if attempt == nil || attempt.SourceKey != selected.Key {
		attempt = &clientstate.HandoverAttempt{SourceKey: selected.Key, RequestID: rand.Text(), Registration: selected.State.Registration}
	}
	candidate.Handover = attempt
	if err := o.persistSessionState(a.key, candidate); err != nil {
		return err
	}
	a.mu.Lock()
	a.state.Handover = attempt
	a.mu.Unlock()
	grant, err := o.exchangeHolder(ctx, b, direct, directCodeRequest{Handover: true, PID: id.PID, CWD: id.CWD, ProcessKey: processKey, deviceCodeRequest: deviceCodeRequest{Harness: id.Harness, SessionID: id.SessionID, ClientID: clientID, RequestID: attempt.RequestID}, RegistrationID: attempt.Registration.RegistrationID, Secret: attempt.Registration.Secret})
	if err != nil {
		var refusal broker.HandoverRefusal
		if !errors.As(err, &refusal) {
			return err
		} // Unknown apply/transport state must retry the same request, never allocate a second registration.
		a.mu.Lock()
		a.state.Handover = nil
		a.mu.Unlock()
		_ = o.persistSession(a)
		return skip(refusal.Reason)
	}
	if grant.Mode != "auto" || grant.RegistrationID != attempt.Registration.RegistrationID {
		return handoverError("refused")
	}
	now := o.now()
	b.mu.Lock()
	d.registrationID, d.name, d.secret, d.revision = grant.RegistrationID, grant.Nickname, attempt.Registration.Secret, grant.Revision
	d.detached = false
	d.handedOver = false
	d.handoverFrom = grant.HandoverFrom
	saved := *d
	b.mu.Unlock()
	candidate.Registration = registrationSnapshot(&saved, id, o.project)
	candidate.Credential = &clientstate.SessionCredential{AccessToken: grant.AccessToken, TokenType: grant.TokenType, IssuedAt: now, ExpiresAt: now.Add(time.Duration(grant.ExpiresIn) * time.Second), SessionGrantID: grant.SessionGrantID, Nickname: grant.Nickname, Scopes: grant.Scopes, AdmissionMode: grant.Mode}
	candidate.Direct = nil
	candidate.Device = nil
	candidate.AutoRecovery = nil
	candidate.Handover = nil
	candidate.LastAdmissionMode = "auto"
	candidate.AdmissionFingerprint = o.admissionFingerprint()
	if err := o.persistSessionState(a.key, candidate); err != nil {
		return err
	}
	a.mu.Lock()
	a.state = candidate
	a.scopesVerified = true
	a.deriveStatus(now)
	a.mu.Unlock()
	if err := o.state.ClearHandedOver(a.key); err != nil {
		return err
	}
	return o.state.MarkHandedOver(selected.Key, a.key)
}
