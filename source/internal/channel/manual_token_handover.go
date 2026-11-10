package channel

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type manualExplicitAttachKey struct{}

// Only a previously started, unfinished device authorization carries intent
// through the follow retry gate. Credentials alone never grant fresh admission.
func deviceAuthorizationInFlight(b *ingressBinding) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	d := b.durable
	b.mu.Unlock()
	if d == nil || d.session == nil {
		return false
	}
	a := d.session
	a.mu.Lock()
	defer a.mu.Unlock()
	device := a.state.Device
	return a.state.Registration.InitialAttempt == initialPending && device != nil && ((a.status == authPending && device.Status == clientstate.DevicePending) || (a.status == authAuthorized && device.Status == clientstate.DeviceRedeemed && a.state.Credential != nil))
}
func (f *claudeFollow) completionRetryBlocked(id string, b *ingressBinding) bool {
	return !deviceAuthorizationInFlight(b) && f.startupRetryBlocked(id)
}

func temporaryHandoverName(name string) string {
	for len(name) > 123 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name + "-next"
}
func (o durableOptions) prepareManualHandover(ctx context.Context, b *ingressBinding, id broker.AttachRequest, source clientstate.SavedConversation) error {
	a := b.durable.session
	a.exchangeMu.Lock()
	defer a.exchangeMu.Unlock()
	a.mu.Lock()
	candidate := a.state
	attempt := candidate.Handover
	a.mu.Unlock()
	explicit, _ := ctx.Value(manualExplicitAttachKey{}).(bool)
	if attempt == nil || !attempt.Manual || attempt.SourceKey != source.Key {
		if !explicit {
			o.diagnose(b, "handover-skipped", "approval-required", "")
			return handoverError("approval-required")
		}
		attempt = &clientstate.HandoverAttempt{Manual: true, SourceKey: source.Key, RequestID: rand.Text(), Registration: source.State.Registration}
	} else {
		copy := *attempt
		attempt = &copy
		if explicit && attempt.StopReason != "" {
			attempt.StopReason = ""
			attempt.RequestID = rand.Text()
		}
	}
	candidate.Handover = attempt
	if err := o.persistSessionState(a.key, candidate); err != nil {
		return err
	}
	a.mu.Lock()
	a.state.Handover = attempt
	a.mu.Unlock()
	if attempt.StopReason != "" {
		return handoverError(attempt.StopReason)
	}
	return nil
}
func (o durableOptions) finishManualHandover(ctx context.Context, b *ingressBinding, id broker.AttachRequest) error {
	a := b.durable.session
	if a == nil {
		return nil
	}
	a.exchangeMu.Lock()
	defer a.exchangeMu.Unlock()
	a.mu.Lock()
	candidate := a.state
	attempt := candidate.Handover
	credential := candidate.Credential
	a.mu.Unlock()
	if attempt == nil || !attempt.Manual {
		return nil
	}
	if attempt.StopReason != "" {
		return handoverError(attempt.StopReason)
	}
	if credential == nil || credential.AdmissionMode != "manual" {
		return handoverError("approval-required")
	}
	request := broker.RebindRequest{RegistrationID: attempt.Registration.RegistrationID, Harness: id.Harness, SessionID: id.SessionID, ClientID: candidate.ClientID, PID: id.PID, CWD: id.CWD, ProcessKey: o.handoverProcessKey(id), Secret: attempt.Registration.Secret, RequestID: attempt.RequestID, ExpectedRevision: attempt.Registration.Revision}
	var result broker.RebindResult
	var err error
	for tries := 0; tries < 3; tries++ {
		probe := o.device.newUpstream(credential.AccessToken)
		result, err = o.manualRebindHolder(ctx, b, probe, request)
		closeUpstream(probe)
		if err == nil {
			break
		}
		lower := strings.ToLower(err.Error())
		if strings.Contains(lower, "unknown tool") || strings.Contains(lower, "tool not found") {
			result.Reason = "unsupported-mode"
			err = nil
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if tries < 2 {
			timer := time.NewTimer(time.Duration(tries+1) * time.Second)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
	}
	if err != nil {
		return err
	} // Unknown outcome keeps the same persisted request ID.
	if result.Reason != "" {
		copy := *attempt
		copy.StopReason = broker.SafeHandoverReason(result.Reason)
		candidate.Handover = &copy
		if err := o.persistSessionState(a.key, candidate); err != nil {
			return err
		}
		a.mu.Lock()
		a.state.Handover = &copy
		a.mu.Unlock()
		o.diagnose(b, "handover-skipped", copy.StopReason, "")
		return handoverError(copy.StopReason)
	}
	if result.RegistrationID != attempt.Registration.RegistrationID || result.Name == "" || result.Revision == 0 {
		return errors.New("manual handover returned invalid proof")
	}
	b.mu.Lock()
	d := b.durable
	d.registrationID = result.RegistrationID
	d.name = result.Name
	d.secret = attempt.Registration.Secret
	d.revision = result.Revision
	d.handedOver = false
	d.handoverFrom = result.HandoverFrom
	d.detached = false
	d.initialAttempt = initialComplete
	b.manualSource = attempt.Registration.SessionID
	saved := *d
	b.mu.Unlock()
	candidate.Registration = registrationSnapshot(&saved, id, o.project)
	copy := *credential
	copy.Nickname = result.Name
	candidate.Credential = &copy
	candidate.Handover = nil
	candidate.Direct = nil
	candidate.AutoRecovery = nil
	candidate.LastAdmissionMode = "manual"
	candidate.AdmissionFingerprint = o.admissionFingerprint()
	if err := o.persistSessionState(a.key, candidate); err != nil {
		return err
	}
	a.mu.Lock()
	a.state = candidate
	a.deriveStatus(o.now())
	a.mu.Unlock()
	if err := o.state.ClearHandedOver(a.key); err != nil {
		return err
	}
	return o.state.MarkHandedOver(attempt.SourceKey, a.key)
}

// Keep unknown-tool classification local, before the ordinary call helper
// deliberately discards arbitrary remote text. No peer text is persisted.
func callManualRebind(ctx context.Context, upstream Upstream, request broker.RebindRequest) (broker.RebindResult, error) {
	response, err := upstream.CallTool(ctx, &mcp.CallToolParams{Name: "rebind_session", Arguments: request})
	missing := func(text string) bool {
		lower := strings.ToLower(text)
		return strings.Contains(lower, "unknown tool") || strings.Contains(lower, "tool not found")
	}
	if err != nil {
		if missing(err.Error()) {
			return broker.RebindResult{Reason: "unsupported-mode"}, nil
		}
		if ctx.Err() != nil {
			return broker.RebindResult{}, ctx.Err()
		}
		return broker.RebindResult{}, errors.New("manual handover transport failed")
	}
	if response == nil {
		return broker.RebindResult{}, errors.New("manual handover returned no result")
	}
	if response.IsError {
		for _, c := range response.Content {
			if text, ok := c.(*mcp.TextContent); ok && missing(text.Text) {
				return broker.RebindResult{Reason: "unsupported-mode"}, nil
			}
		}
		return broker.RebindResult{}, errors.New("manual handover request rejected")
	}
	var data []byte
	if response.StructuredContent != nil {
		data, err = json.Marshal(response.StructuredContent)
	} else if len(response.Content) > 0 {
		if c, ok := response.Content[0].(*mcp.TextContent); ok {
			data = []byte(c.Text)
		}
	}
	var result broker.RebindResult
	if err != nil || len(data) == 0 || json.Unmarshal(data, &result) != nil {
		return broker.RebindResult{}, errors.New("manual handover returned invalid proof")
	}
	return result, nil
}

// Publication is joined by the ingress worker group and never blocks a receiver
// on f.mu. It also transfers an envelope held by the previous conversation.
func (f *claudeFollow) publishManualHandover(ctx context.Context, sourceID string, id broker.AttachRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	target := f.lookup(id.SessionID)
	if target == nil {
		return
	}
	target.mu.Lock()
	ready := target.started && !target.failed
	target.mu.Unlock()
	if !ready {
		return
	}
	source := f.lookup(sourceID)
	if source != nil && source != target {
		source.mu.Lock()
		source.retired, source.selfStopped = true, false
		if source.durable != nil {
			source.durable.handedOver = true
		}
		cancel, done := source.cancel, source.done
		source.followStopped = false
		source.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				return
			}
		}
		f.deliverPending(ctx, source, target)
	}
	f.current = id
	f.clearRetry()
	f.result = "followed: handover"
}

func (f *claudeFollow) manualApprovalRequired(b *ingressBinding, id broker.AttachRequest) bool {
	if b == nil || b.durable == nil || b.durable.session == nil {
		return false
	}
	a := b.durable.session
	a.mu.Lock()
	manual := a.state.LastAdmissionMode == "manual" || (a.state.Credential != nil && a.state.Credential.AdmissionMode == "manual")
	a.mu.Unlock()
	if !manual {
		return false
	}
	if f.opts.state == nil {
		return true
	}
	cid, err := f.opts.state.ClientID()
	if err != nil {
		return true
	}
	state, found, err := f.opts.state.LoadSession(clientstate.SessionKey(f.opts.scope, id.Harness, id.SessionID, cid))
	return err != nil || !found || state.Registration.RegistrationID == "" || state.Registration.HandedOverTo != ""
}

func (o durableOptions) manualRebindHolder(ctx context.Context, b *ingressBinding, u Upstream, r broker.RebindRequest) (broker.RebindResult, error) {
	for {
		r.Holder = o.currentHolder(ctx)
		o.diagnoseDeviceIdentity(b)
		result, err := callManualRebind(ctx, u, r)
		if err != nil || result.Reason != broker.CodeRegistrationInUse {
			if err == nil {
				o.clearHolderWait(b)
			}
			return result, err
		}
		proof, err := o.waitHolder(ctx, b, result.HolderChallenge)
		if err != nil {
			return broker.RebindResult{}, err
		}
		r.HolderProof = proof
	}
}
