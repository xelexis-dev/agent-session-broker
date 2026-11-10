package channel

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// deviceAuthorizer is the broker's device authorization API as the client
// uses it. The production implementation is deviceAuthClient.
type deviceAuthorizer interface {
	requestCode(ctx context.Context, r deviceCodeRequest) (deviceAuthorization, error)
	redeem(ctx context.Context, deviceCode string) (sessionGrant, tokenOutcome, error)
}

// deviceAuthOptions turns the local client into a project-token client: each
// conversation obtains its own session credential through device
// authorization, stores it under the auth directory, and talks to the broker
// through its own upstream connection carrying only that credential.
type deviceAuthOptions struct {
	nickname    string
	authorizer  deviceAuthorizer
	newUpstream func(credential string) Upstream
	clock       devicePollClock
	// persist replaces the session state write; nil writes through clientstate.
	persist func(*clientstate.Store, string, clientstate.SessionState) error
}

// Authorization status of one conversation as reported by auth_status.
const (
	authUnauthorized = "unauthorized"
	authPending      = "pending"
	authAuthorized   = "authorized"
	authDenied       = "denied"
	authExpired      = "expired"
	authRejected     = "rejected"
	authFailed       = "failed"
)

// errAuthorizationPending means the conversation has no session credential
// yet; the pending code is shown to the user through auth_status and every
// forwarded tool answer until approval.
var errAuthorizationPending = errors.New("device authorization is pending user approval")

// sessionAuth is the in-memory view of one conversation's SessionState. Its
// mutex covers state, status and polling; the binding mutex is not held while
// touching it, so the poller and tool handlers never deadlock on each other.
type sessionAuth struct {
	recoveryDone             chan struct{}
	recoveryCancel           context.CancelFunc
	holderWaiting            bool
	registrationAttempts     int
	explicitAttaching        int
	exchangeMu               sync.Mutex
	mu                       sync.Mutex
	key                      string
	state                    clientstate.SessionState
	status                   string
	message                  string
	scopesVerified           bool
	recovering               bool
	polling                  bool
	pollCancel               context.CancelFunc
	pollDone                 <-chan struct{}
	diagnostic               func(string, time.Time)
	diagnosticRegistrationID string
	diagnosticGeneration     uint64
}

func (a *sessionAuth) deriveStatus(now time.Time) {
	previous := a.status
	defer func() {
		if previous != a.status && a.diagnostic != nil {
			a.diagnostic(a.status, now)
		}
	}()
	device := a.state.Device
	if a.state.Registration.RegistrationID == "" && a.state.Registration.InitialAttempt == initialPending && device != nil {
		if device.Status == clientstate.DeviceDenied || device.Status == clientstate.DeviceExpired || device.Status == clientstate.DeviceFailed || (device.Status == clientstate.DevicePending && !now.Before(device.ExpiresAt)) {
			a.state.Registration.InitialAttempt = initialStopped
		}
	}
	switch {
	case a.state.Credential != nil:
		a.status = authAuthorized
	case device == nil, device.Status == clientstate.DeviceRequested:
		a.status = authUnauthorized
	case device.Status == clientstate.DevicePending && now.Before(device.ExpiresAt):
		a.status = authPending
	case device.Status == clientstate.DevicePending:
		a.status = authExpired
		a.message = "approval-expired: The approval code expired. Call attach_session to request a new approval code, then ask the user to approve it."
	case device.Status == clientstate.DeviceDenied:
		a.status = authDenied
		a.message = "approval-denied: The approval request was denied. Call attach_session to request a new approval code if the user wants to try again."
	case device.Status == clientstate.DeviceExpired:
		a.status = authExpired
		a.message = "approval-expired: The approval code expired. Call attach_session to request a new approval code, then ask the user to approve it."
	case device.Status == clientstate.DeviceRejected:
		a.status = authRejected
		a.message = credentialGuidance(true)
	case device.Status == clientstate.DeviceFailed:
		a.status = authFailed
		a.message = "authorization-failed: Broker authorization failed. Call attach_session to request a new approval code."
	default:
		a.status = authUnauthorized
	}
}

// pendingText is the user-facing instruction while approval is outstanding.
func (a *sessionAuth) pendingText() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pendingTextLocked()
}

func (a *sessionAuth) pendingTextLocked() string {
	if a.holderWaiting {
		return registrationInUseGuidance
	}
	if a.state.Registration.Detached && a.explicitAttaching == 0 {
		return detachedStatusMessage("session-detached: This conversation is disconnected.")
	}
	if !registrationPermitted(a.state.Registration) && a.state.Registration.InitialAttempt == "" && !a.state.Registration.Detached {
		return conversationNotRegistered
	}
	if a.autoEligibleLocked() || a.state.AutoRecovery != nil {
		if a.recovering {
			return "credential-rejected: automatic reauthorization is in progress. The existing registration is preserved."
		}
		if r := a.state.AutoRecovery; r != nil && r.StopReason != "" {
			return autoRecoveryGuidance(r.StopReason)
		}
	}
	switch a.status {
	case authPending:
		message := "approval-pending: This session is waiting for user approval. Call auth_status for the approval details and ask the user to review and approve the request. Registration continues automatically after approval."
		if d := a.state.Device; d != nil {
			if link := approvalReadyLink(d.VerificationURI, d.VerificationURIComplete, d.UserCode, d.RequestedNickname); link != "" {
				message += " Open approval link: " + link
			}
		}
		return message
	case authAuthorized:
		return "this session is authorized; registration is in progress"
	default:
		if a.message != "" {
			return a.message
		}
		return conversationNotRegistered + ". " + registrationInstructions
	}
}

func (o durableOptions) persistSession(a *sessionAuth) error {
	a.mu.Lock()
	copied := a.state
	if copied.Credential != nil {
		credential := *copied.Credential
		copied.Credential = &credential
	}
	if copied.AutoRecovery != nil {
		recovery := *copied.AutoRecovery
		copied.AutoRecovery = &recovery
	}
	if copied.Direct != nil {
		direct := *copied.Direct
		copied.Direct = &direct
	}
	if copied.Device != nil {
		device := *copied.Device
		copied.Device = &device
	}
	a.mu.Unlock()
	return o.persistSessionState(a.key, copied)
}

// persistSessionState writes a caller-owned snapshot without making it visible
// to concurrent tool requests through sessionAuth.
func (o durableOptions) persistSessionState(key string, state clientstate.SessionState) error {
	if o.device != nil && o.device.persist != nil {
		return o.device.persist(o.state, key, state)
	}
	return o.state.SaveSession(key, state)
}

// loadSessionRegistration opens one conversation's session state under its
// own lock, adopting a legacy registration file once so the same conversation
// keeps its registration id and resume secret across the token change.
func (o durableOptions) loadSessionRegistration(identity broker.AttachRequest) (*durableRegistration, func(), error) {
	clientID, err := o.state.ClientID()
	if err != nil {
		return nil, nil, err
	}
	key := clientstate.SessionKey(o.scope, identity.Harness, identity.SessionID, clientID)
	unlock, err := o.state.LockSession(key)
	if err != nil {
		return nil, nil, err
	}
	state, found, err := o.state.LoadSession(key)
	if err == nil && !found {
		if _, err = o.state.AdoptLegacyRegistration(key, clientstate.RegistrationKey(o.scope, identity.Harness, identity.SessionID)); err == nil {
			state, found, err = o.state.LoadSession(key)
		}
	}
	if err != nil {
		unlock()
		return nil, nil, err
	}
	if !found {
		state = clientstate.SessionState{Registration: clientstate.Registration{Secret: clientstate.NewSecret()}}
	}
	unresolvedHolder := false
	// Legacy adoption has no scoped identity until its first ordinary save.
	// Existing scoped states must resolve even when the source marker was never written.
	if found && identity.Harness == "claude" && state.Scope != "" && state.ClientID != "" && state.Registration.RegistrationID != "" && !state.Registration.Detached {
		holder, resolveErr := o.state.HandoverHolder(key, o.scope, identity.Harness, clientID, false)
		if resolveErr != nil {
			reason := handoverResolutionReason(resolveErr)
			_ = o.state.RecordDiagnostic(key, clientstate.DiagnosticEvent{At: o.now().UTC(), SessionID: identity.SessionID, RegistrationID: state.Registration.RegistrationID, Event: "handover-skipped", Reason: reason})
			// Keep auth_status and its diagnostic available, while fencing both
			// ordinary resume and any fallback to fresh admission for this binding.
			unresolvedHolder = true
		} else {
			state.Registration.HandedOverTo = ""
			if holder.Key != key {
				state.Registration.HandedOverTo = holder.Key
			}
		}
	}
	state.Scope, state.Project, state.Harness, state.SessionID, state.ClientID = o.scope, o.project, identity.Harness, identity.SessionID, clientID
	if state.Credential != nil && state.Credential.AdmissionMode != "" {
		state.LastAdmissionMode = state.Credential.AdmissionMode
	}
	inferInitialAttempt(&state, o.now())
	saved := state.Registration
	d := &durableRegistration{
		parentPID: identity.PID, processKey: o.handoverProcessKey(identity), handedOver: saved.HandedOverTo != "" || unresolvedHolder, handoverFrom: saved.HandoverFrom, detached: saved.Detached, initialAttempt: saved.InitialAttempt, key: key, clientID: clientID, registrationID: saved.RegistrationID, name: saved.Name, secret: saved.Secret, revision: saved.Revision,
		pendingRequest: saved.PendingRequestID, pendingLease: saved.PendingLease, pendingRevision: saved.PendingRevision,
	}
	d.session = &sessionAuth{key: key, state: state, diagnosticRegistrationID: saved.RegistrationID}
	o.resetSupersededProjectSource(d.session)
	d.session.diagnostic = func(status string, at time.Time) {
		_ = o.state.RecordDiagnostic(key, clientstate.DiagnosticEvent{At: at.UTC(), SessionID: identity.SessionID, RegistrationID: d.session.diagnosticRegistrationID, Generation: d.session.diagnosticGeneration, Event: "auth-transition", AuthState: status})
	}
	if err := o.transitionAdmission(d.session); err != nil {
		unlock()
		return nil, nil, err
	}
	d.session.deriveStatus(o.now())
	return d, unlock, nil
}

func (o durableOptions) saveSessionRegistration(identity broker.AttachRequest, d *durableRegistration) error {
	a := d.session
	a.mu.Lock()
	a.state.Registration = registrationSnapshot(d, identity, o.project)
	if a.state.Credential != nil && d.name != "" {
		credential := *a.state.Credential
		credential.Nickname = d.name
		a.state.Credential = &credential
	}
	a.mu.Unlock()
	return o.persistSession(a)
}

func (o durableOptions) now() time.Time {
	if o.device != nil && o.device.clock.now != nil {
		return o.device.clock.now()
	}
	return time.Now()
}

// requestDeviceCode asks the broker for a new approval code for this
// conversation and stores it as pending before anyone sees it. A pending
// request id is reused so a lost answer does not leave two live codes.
func (o durableOptions) requestDeviceCode(ctx context.Context, a *sessionAuth, identity broker.AttachRequest) error {
	if err := o.checkScopedAdmission(ctx, a); err != nil {
		return err
	}
	a.mu.Lock()
	requestID := ""
	if d := a.state.Device; d != nil && (d.Status == clientstate.DeviceRequested || d.Status == clientstate.DevicePending) {
		requestID = d.RequestID
	}
	if requestID == "" {
		requestID = rand.Text()
	}
	clientID := a.state.ClientID
	// Record the id before the request leaves: a lost answer or a restart then
	// retries with the same id and the broker replaces the pending code instead
	// of adding a second one. A denied or failed request keeps its own id, so
	// the next explicit request starts fresh.
	a.state.Credential = nil
	a.state.Direct = nil
	a.state.Device = &clientstate.DeviceAuthorization{RequestID: requestID, Status: clientstate.DeviceRequested}
	a.deriveStatus(o.now())
	a.mu.Unlock()
	if err := o.persistSession(a); err != nil {
		return err
	}
	issued, err := o.device.authorizer.requestCode(ctx, deviceCodeRequest{Harness: identity.Harness, SessionID: identity.SessionID, ClientID: clientID, RequestID: requestID, CWDBasename: path.Base(strings.ReplaceAll(identity.CWD, "\\", "/"))})
	now := o.now()
	a.mu.Lock()
	if err != nil {
		if errors.Is(err, errServerUpgradeRequired) || errors.Is(err, errProjectTokenRejected) {
			a.state.Device = &clientstate.DeviceAuthorization{RequestID: requestID, Status: clientstate.DeviceFailed, Error: err.Error()}
			a.deriveStatus(now)
			a.mu.Unlock()
			_ = o.persistSession(a)
			return err
		}
		a.mu.Unlock()
		return err
	}
	nickname := identity.Name
	if nickname == "" {
		nickname = o.device.nickname
	}
	a.state.Credential = nil
	a.state.Device = &clientstate.DeviceAuthorization{
		VerificationURIComplete: issued.VerificationURIComplete, RequestedNickname: nickname,
		RequestID: requestID, DeviceCode: issued.DeviceCode, UserCode: issued.UserCode, VerificationURI: issued.VerificationURI,
		ExpiresAt: now.Add(time.Duration(issued.ExpiresIn) * time.Second), Interval: issued.Interval, Status: clientstate.DevicePending,
	}
	a.deriveStatus(now)
	a.mu.Unlock()
	return o.persistSession(a)
}

// awaitDeviceApproval polls until the broker answers. On success the
// credential is persisted before anything else happens; a failed write leaves
// the conversation unauthorized so no MCP request ever carries an unsaved
// credential. It returns true when a credential is ready to use.
func (o durableOptions) awaitDeviceApproval(ctx context.Context, a *sessionAuth) bool {
	a.mu.Lock()
	device := a.state.Device
	if device == nil || device.Status != clientstate.DevicePending {
		a.mu.Unlock()
		return false
	}
	code, interval, expires := device.DeviceCode, time.Duration(device.Interval)*time.Second, device.ExpiresAt
	a.mu.Unlock()
	granted, outcome, err := pollDeviceToken(ctx, o.device.authorizer, code, interval, expires, o.device.clock)
	if ctx.Err() != nil {
		return false
	}
	now := o.now()
	a.mu.Lock()
	switch outcome {
	case tokenGranted:
		candidate := a.state
		candidate.LastAdmissionMode = "manual"
		candidate.Credential = &clientstate.SessionCredential{AdmissionMode: "manual", AccessToken: granted.AccessToken, TokenType: granted.TokenType, IssuedAt: now, SessionGrantID: granted.SessionGrantID, Nickname: granted.Nickname}
		if granted.ExpiresIn > 0 {
			candidate.Credential.ExpiresAt = now.Add(time.Duration(granted.ExpiresIn) * time.Second)
		}
		device := *a.state.Device
		device.Status = clientstate.DeviceRedeemed
		candidate.Device = &device
		a.mu.Unlock()
		if err := o.persistSessionState(a.key, candidate); err != nil {
			a.mu.Lock()
			a.state.Credential = nil
			a.state.Device.Status = clientstate.DeviceFailed
			a.state.Device.Error = "the session credential could not be saved locally; MCP was not started"
			a.deriveStatus(now)
			a.mu.Unlock()
			return false
		}
		a.mu.Lock()
		// Keep the persisted snapshot independent of later live-state updates.
		liveCredential, liveDevice := *candidate.Credential, *candidate.Device
		a.state.LastAdmissionMode = candidate.LastAdmissionMode
		a.state.Credential = &liveCredential
		a.state.Device = &liveDevice
		a.deriveStatus(now)
		a.mu.Unlock()
		return true
	case tokenDenied:
		a.state.Device.Status = clientstate.DeviceDenied
	case tokenExpired:
		a.state.Device.Status = clientstate.DeviceExpired
	case tokenInvalidGrant:
		a.state.Device.Status = clientstate.DeviceFailed
		a.state.Device.Error = "the broker no longer recognises this approval code"
	case tokenInvalidClient:
		a.state.Device.Status = clientstate.DeviceFailed
		a.state.Device.Error = "the broker rejected the project token"
		if terminalProjectError(err) {
			a.state.Device.Error = err.Error()
			a.message = err.Error()
		}
	default:
		a.state.Device.Status = clientstate.DeviceFailed
		if err != nil {
			a.state.Device.Error = err.Error()
		}
	}
	a.deriveStatus(now)
	a.mu.Unlock()
	_ = o.persistSession(a)
	return false
}

// rejectCredential records that the broker refused the stored credential.
// Automatic recovery stops here; only an explicit attach_session requests a
// new approval code.
func (o durableOptions) rejectCredential(a *sessionAuth) {
	o.rejectCredentialIfCurrent(a, "")
}

// A retired receiver must not reject a newer credential approved while it
// was stopping. Empty expected preserves the explicit rejection path.
func (o durableOptions) rejectCredentialIfCurrent(a *sessionAuth, expected string) {
	a.mu.Lock()
	if a.state.Credential == nil || (expected != "" && a.state.Credential.AccessToken != expected) {
		a.mu.Unlock()
		return
	}
	if a.state.Credential.AdmissionMode != "" {
		a.state.LastAdmissionMode = a.state.Credential.AdmissionMode
	}
	a.state.Credential = nil
	if a.state.Device == nil {
		a.state.Device = &clientstate.DeviceAuthorization{}
	}
	a.state.Device.Status = clientstate.DeviceRejected
	a.deriveStatus(o.now())
	a.mu.Unlock()
	_ = o.persistSession(a)
}

type credentialRejecter interface {
	credentialRejected() bool
}

func upstreamRejected(u Upstream) bool {
	r, ok := u.(credentialRejecter)
	return ok && r.credentialRejected()
}

// authStatus is the auth_status tool result.
type clientUpdate struct {
	Available   bool   `json:"available"`
	Current     string `json:"current"`
	Recommended string `json:"recommended"`
	Steps       string `json:"steps"`
}

type authStatus struct {
	ParentDiagnostic        *parentDiagnostic              `json:"parentDiagnostic,omitempty"`
	ToolProfile             string                         `json:"toolProfile"`
	ToolProfileNote         string                         `json:"toolProfileNote"`
	LaunchHint              string                         `json:"launchHint,omitempty"`
	ReceiverReady           *bool                          `json:"receiverReady,omitempty"`
	CodexDelivery           *codexDelivery                 `json:"codexDelivery,omitempty"`
	IdentitySource          string                         `json:"identitySource,omitempty"`
	IdentityConfirmed       *bool                          `json:"identityConfirmed,omitempty"`
	DeviceIdentity          string                         `json:"deviceIdentity,omitempty"`
	DeviceIdentityHint      string                         `json:"deviceIdentityHint,omitempty"`
	Update                  *clientUpdate                  `json:"update,omitempty"`
	Delivery                string                         `json:"delivery,omitempty"`
	PendingMessages         int                            `json:"pendingMessages,omitempty"`
	HandoverFrom            *broker.HandoverFrom           `json:"handoverFrom,omitempty"`
	ScopeDetails            []access.ScopeDetail           `json:"scopeDetails,omitempty"`
	ScopeSource             string                         `json:"scopeSource"`
	CredentialExpiresAt     string                         `json:"credentialExpiresAt,omitempty"`
	Compatible              bool                           `json:"compatible"`
	RequiredClient          string                         `json:"requiredClient,omitempty"`
	RequiredServer          string                         `json:"requiredServer,omitempty"`
	ReceiveError            string                         `json:"receiveError,omitempty"`
	VerificationURIComplete string                         `json:"verificationUriComplete,omitempty"`
	ClientVersion           string                         `json:"clientVersion"`
	ServerVersion           string                         `json:"serverVersion"`
	ServerVersionKnown      bool                           `json:"serverVersionKnown"`
	Diagnostics             *clientstate.DiagnosticSummary `json:"diagnostics,omitempty"`
	Status                  string                         `json:"status"`
	Message                 string                         `json:"message,omitempty"`
	UserCode                string                         `json:"userCode,omitempty"`
	VerificationURI         string                         `json:"verificationUri,omitempty"`
	ExpiresAt               string                         `json:"expiresAt,omitempty"`
	Name                    string                         `json:"name,omitempty"`
	Registered              bool                           `json:"registered"`
	RegistrationID          string                         `json:"registrationId,omitempty"`
	Scopes                  []string                       `json:"scopes,omitempty"`
	AdmissionMode           string                         `json:"admissionMode,omitempty"`
}

func (a *sessionAuth) report(registered bool, name, registrationID string) authStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := authStatus{HandoverFrom: a.state.Registration.HandoverFrom, AdmissionMode: a.state.LastAdmissionMode, Status: a.status, Message: a.pendingTextLocked(), Registered: registered, Name: name, RegistrationID: registrationID}
	if !registrationPermitted(a.state.Registration) && a.state.Registration.InitialAttempt == "" && !a.state.Registration.Detached {
		out.Status = authUnauthorized
		out.Message = conversationNotRegistered
		out.Registered = false
		return out
	}
	if a.state.Credential != nil {
		out.Scopes = append([]string(nil), a.state.Credential.Scopes...)
		out.AdmissionMode = a.state.Credential.AdmissionMode
		if !a.state.Credential.ExpiresAt.IsZero() {
			out.CredentialExpiresAt = a.state.Credential.ExpiresAt.UTC().Format(time.RFC3339)
		}
	}
	if a.status == authAuthorized && !a.recovering && (a.state.AutoRecovery == nil || a.state.AutoRecovery.StopReason == "") {
		out.Message = "session-authorized: This session has a stored broker credential."
		if !registered && (a.registrationAttempts > 0 || a.state.Registration.InitialAttempt == initialPending) {
			out.Message += " Registration is in progress."
		}
	}
	if a.state.Registration.Detached && a.explicitAttaching == 0 && !registered {
		out.Message = detachedStatusMessage("session-detached: This conversation is disconnected.")
	}
	if a.status == authPending && a.state.Device != nil {
		out.VerificationURIComplete = approvalReadyLink(a.state.Device.VerificationURI, a.state.Device.VerificationURIComplete, a.state.Device.UserCode, a.state.Device.RequestedNickname)
		out.UserCode, out.VerificationURI, out.ExpiresAt = a.state.Device.UserCode, a.state.Device.VerificationURI, a.state.Device.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if a.status == authAuthorized && a.state.Credential != nil && out.Name == "" {
		out.Name = a.state.Credential.Nickname
	}
	if a.holderWaiting {
		out.Message = registrationInUseGuidance
	}
	return out
}

// Approval URLs carry only a user code and the requested label, never bearer
// credentials or arbitrary query parameters supplied in a response/state file.
func approvalReadyLink(base, complete, code, nickname string) string {
	raw := base
	if complete != "" {
		raw = complete
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || code == "" {
		return ""
	}
	u.RawQuery = url.Values{"code": {code}}.Encode()
	u.Fragment = ""
	if nickname != "" && len(nickname) <= 128 {
		q := u.Query()
		q.Set("name", nickname)
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func (o durableOptions) updateApprovalNickname(a *sessionAuth, nickname string) {
	if nickname == "" {
		return
	}
	a.mu.Lock()
	if a.state.Device != nil {
		a.state.Device.RequestedNickname = nickname
	}
	a.mu.Unlock()
	// Keep a late optional hint volatile: writing a pending snapshot here could
	// race the poller committing the newly issued credential.
}
