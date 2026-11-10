package broker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

// Wire result codes retain compatibility with the neutral protocol package.
const (
	CodeInvalid          = protocol.CodeInvalid
	CodeNotFound         = protocol.CodeNotFound
	CodeDetached         = protocol.CodeDetached
	CodeSuspended        = protocol.CodeSuspended
	CodeOwned            = protocol.CodeOwned
	CodeRevisionMismatch = protocol.CodeRevisionMismatch
	CodeStaleProbe       = protocol.CodeStaleProbe
	CodeStorage          = protocol.CodeStorage
	CodeCapacity         = protocol.CodeCapacity
	CodeInvalidToken     = protocol.CodeInvalidToken
)

const (
	defaultProbeAfter            = time.Hour
	defaultProbeWindow           = 10 * time.Minute
	defaultBootGrace             = 15 * time.Minute
	defaultSweepInterval         = 30 * time.Second
	defaultPersistInterval       = time.Minute
	fencedLeaseHistory           = 64
	missingRegistrationRetention = 30 * 24 * time.Hour
	missingExpireBudget          = 16
)

var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)

// Registration DTO aliases are shared by remote services and local clients.
type RegisterRequest = protocol.RegisterRequest
type ResumeRequest = protocol.ResumeRequest
type RenewRequest = protocol.RenewRequest
type ProbeAnswer = protocol.ProbeAnswer
type PendingProbe = protocol.PendingProbe
type RegisterResult = protocol.RegisterResult
type ProbeResult = protocol.ProbeResult

// Admission decides the communication name for a binding in token-protected
// projects. Admit is called outside the service lock, must not bind anything,
// and returns the enforced name (UUID-shaped names issued by the UI are
// accepted) or a rejection code such as CodeInvalidToken.
//
// An admission that also implements reservedAdmission can answer per request
// whether a token grant applies. Requests without a grant then follow the open
// policy (requested name, UUID nickname on conflict) and legacy attach stays
// available. Without that method every request is treated as reserved.
//
// An admission that also implements claimingAdmission is asked to bind the
// grant only after the request proved ownership of any existing registration,
// passed the revision and lease checks, and the name is available; a non-empty
// code rejects and nothing is persisted. Claim runs outside the service lock
// and must not call back into the service.
type Admission interface {
	Admit(ctx context.Context, request RegisterRequest) (name string, code string, err error)
}

type reservedAdmission interface {
	Reserved(ctx context.Context) bool
}

type claimingAdmission interface {
	Claim(ctx context.Context, request RegisterRequest) (code string, err error)
}

// authorizingAdmission gates operations on an existing registration: sending
// as it, waiting on it, renewing or resuming it, answering its probe, reporting
// its ingress failure and detaching it. CheckSession runs inside the service
// lock right after the selector resolved, so a rename or lease change cannot
// slip between the check and the operation. It must be quick, in-memory, and
// must not call back into the service. Read-only listing is not gated.
type authorizingAdmission interface {
	CheckSession(ctx context.Context, session Session) error
}

func (s *RemoteService) authorizeLocked(ctx context.Context, e *remoteSession) error {
	return s.authorizeSessionLocked(ctx, s.snapshot(e))
}

func (s *RemoteService) authorizeSessionLocked(ctx context.Context, session Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.admission == nil {
		return nil
	}
	a, ok := s.admission.(authorizingAdmission)
	if !ok {
		return nil
	}
	return a.CheckSession(ctx, session)
}

// DurableRegistrar is implemented by backends that own durable registrations.
type DurableRegistrar interface {
	RegisterSession(context.Context, RegisterRequest) (RegisterResult, error)
	ResumeSession(context.Context, ResumeRequest) (RegisterResult, error)
	RenewLease(context.Context, RenewRequest) (RegisterResult, error)
	AnswerProbe(context.Context, ProbeAnswer) (ProbeResult, error)
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func secretMatches(hash, secret string) bool {
	return subtle.ConstantTimeCompare([]byte(hash), []byte(hashSecret(secret))) == 1
}

func printableToken(value string, min, max int) bool {
	if len(value) < min || len(value) > max {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] <= ' ' || value[i] > '~' {
			return false
		}
	}
	return true
}

func validRemoteCWD(cwd string) bool {
	return len(cwd) <= 4096 && !strings.ContainsRune(cwd, 0) && (path.IsAbs(cwd) || remoteWindowsAbsolute.MatchString(cwd) || strings.HasPrefix(cwd, `\\`))
}

func validLeaseID(id string) bool { return id != "" && len(id) <= 128 }

// Nicknames are visible routing labels. Invisible format characters (including
// zero-width and bidi controls) are rejected just like the panel nickname input.
// Existing stored registrations remain readable; this only gates new input.
func invalidRemoteNameRune(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf)
}

// validDurableName accepts UUID-shaped names because durable routing resolves
// registration ids and names before session ids; requested names still go
// through validRemoteName. No name may look like a registration id.
func validDurableName(name string) bool {
	return !protocol.IsAdminSender(name) && name != "" && len(name) <= 128 && strings.TrimSpace(name) == name && strings.IndexFunc(name, invalidRemoteNameRune) < 0 && !registrationIDPattern.MatchString(name) && !typedSelector(name)
}

func newUUIDNickname() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// SetAdmission installs the token-mode name policy. Without it the project is
// open: the requested name is used and a conflict yields a UUID nickname.
func (s *RemoteService) SetAdmission(a Admission) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.admission = a
}

// SetProbeSchedule overrides how long a registration may stay missing before
// a probe is issued, how long the probe may stay unanswered, and the grace
// after a server start before a probe is considered unanswered. A zero after or
// window keeps the current value; the grace is always replaced.
func (s *RemoteService) SetProbeSchedule(after, window, bootGrace time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if after > 0 {
		s.probeAfter = after
	}
	if window > 0 {
		s.probeWindow = window
	}
	s.bootGrace = bootGrace
}

func (s *RemoteService) reservedLocked(ctx context.Context) (Admission, bool) {
	if s.admission == nil {
		return nil, false
	}
	if r, ok := s.admission.(reservedAdmission); ok && !r.Reserved(ctx) {
		return nil, false
	}
	return s.admission, true
}

func (s *RemoteService) durableIdentityLocked(harness, sessionID, clientID string) *remoteSession {
	for _, e := range s.byRegistration {
		if e.record.Harness == harness && e.record.SessionID == sessionID && e.record.ClientID == clientID {
			return e
		}
	}
	return nil
}

func (s *RemoteService) fenceLease(e *remoteSession, id string) {
	if id == "" {
		return
	}
	e.leases[id] = struct{}{}
	if e.record != nil {
		e.leaseOrder = append(e.leaseOrder, id)
		for len(e.leaseOrder) > fencedLeaseHistory {
			delete(e.leases, e.leaseOrder[0])
			e.leaseOrder = e.leaseOrder[1:]
		}
	}
}

func (s *RemoteService) issueLeaseLocked(e *remoteSession, lease string, now time.Time, capabilities []string) {
	e.protectAckLoss(now)
	e.requeueInflight()
	e.messageAck = protocol.SupportsMessageAck(capabilities)
	e.messageAttachment = protocol.SupportsAttachment(capabilities)
	s.fenceLease(e, e.identity.ChannelID)
	s.fenceLease(e, lease)
	e.identity.ChannelID = lease

	e.runtimeInfo, e.runtimeAuthorization = nil, nil
	e.runtimeSequence = 0
	e.runtimeObservedAt = time.Time{}
	e.activitySequence = 0
	e.activityObservedAt = time.Time{}
	e.clearActivity("evidence-missing")
	e.retired = false
	e.queueHeldUntil = time.Time{}
	e.until = now.Add(remoteLeaseDuration)
	e.session.Generation = e.record.LeaseGeneration
	e.session.Status = StatusAlive
	e.session.ErrorCode = ""
	e.session.AutomaticDeliveryReady = true
	s.notify(e)
}

func (s *RemoteService) durableSnapshot(e *remoteSession) Session { return s.snapshot(e) }

func (s *RemoteService) pendingProbe(e *remoteSession) *PendingProbe {
	if e.record.Probe == nil {
		return nil
	}
	return &PendingProbe{ID: e.record.Probe.ID, Revision: e.record.Probe.Revision, Deadline: s.effectiveDeadline(e.record.Probe.Deadline)}
}

func (s *RemoteService) effectiveDeadline(deadline time.Time) time.Time {
	if grace := s.bootedAt.Add(s.bootGrace); grace.After(deadline) {
		return grace
	}
	return deadline
}

func (s *RemoteService) persistLocked(record *registrationRecord) error {
	if s.store != nil {
		if err := s.store.save(record); err != nil {
			return err
		}
	}
	if e := s.byRegistration[record.RegistrationID]; e != nil {
		e.dirty = false
	}
	return nil
}

// removeLocked drops a registration from memory after its file is gone. When
// the file cannot be removed the record stays and the next sweep retries.
func (s *RemoteService) removeLocked(e *remoteSession, code string) error {
	return s.removeEntriesLocked([]*remoteSession{e}, code)
}

func (s *RemoteService) removeEntriesLocked(entries []*remoteSession, code string) error {
	if len(entries) == 0 {
		return nil
	}
	var proofs []access.DetachedRemovalProof
	var ids []string
	for _, e := range entries {
		if e.record == nil {
			continue
		}
		r := e.record
		ids = append(ids, r.RegistrationID)
		if r.Detached {
			proofs = append(proofs, access.DetachedRemovalProof{RegistrationID: r.RegistrationID, Revision: r.Revision, Identity: bindingForRename(e), OwnerID: r.OwnerID, Name: r.Name, AdmissionMode: r.AdmissionMode, GrantID: r.AdmissionGrantID, ParentID: r.AdmissionParentID, Detached: r.Detached, ReattachAfterCleanup: r.ReattachAfterCleanup})
		}
	}
	if len(proofs) > 0 {
		if s.detachedCleanupBatch != nil {
			if err := s.detachedCleanupBatch(proofs); err != nil {
				return err
			}
		}
	}
	if s.store != nil {
		if s.store.database != nil {
			if err := s.store.database.Batch(nil, ids); err != nil {
				return err
			}
		} else {
			for _, id := range ids {
				if err := s.store.remove(id); err != nil {
					return err
				}
			}
		}
	}
	for _, e := range entries {
		e.observedPeers = nil
		if s.sessions[e.session.Name] == e {
			delete(s.sessions, e.session.Name)
		}
		if e.record != nil {
			delete(s.byRegistration, e.record.RegistrationID)
			s.retire(e, StatusMissing, code)
		}
	}

	return nil
}

func (s *RemoteService) uniqueNicknameLocked() string {
	for {
		candidate := newUUIDNickname()
		if s.find(candidate) == nil {
			return candidate
		}
	}
}

// nameHolderBlocks reports whether an existing entry keeps a name from a new
// or renamed registration. Retired legacy attachments release their name.
func (s *RemoteService) nameHolderBlocks(holder, self *remoteSession) bool {
	if holder == nil || holder == self {
		return false
	}
	return holder.record != nil || !holder.retired
}

func (s *RemoteService) validateRegister(r RegisterRequest) bool {
	return !protocol.IsAdminSender(r.Name) && (r.Harness == "claude" || r.Harness == "codex" || r.Harness == "kimi") && validCanonicalConversationID(r.SessionID) && r.PID > 0 &&
		validRemoteCWD(r.CWD) && clientIDPattern.MatchString(r.ClientID) && validLeaseID(r.ChannelID) &&
		printableToken(r.Secret, 32, 256) && printableToken(r.RequestID, 1, 128) && len(r.Token) <= 4096 &&
		len(r.ProcessKey) <= 512 && len(r.MAC) <= 64 && protocol.ValidReceiverHolder(r.Holder)
}

type registerPlan struct {
	existing       *remoteSession
	replay         bool
	sameLease      bool
	name           string
	requested      string
	warning        string
	registrationID string
}

// planRegisterLocked validates a register request against current state
// without changing it. It runs before and, when a claim callback exists, again
// after the callback so a concurrent change between the two is caught. The
// grant check (CheckSession) runs on the final pass only, after Claim had the
// chance to bind a new grant to this identity; a replay is checked always so a
// revoked grant never obtains a live-looking answer from an old request id.
func (s *RemoteService) planRegisterLocked(ctx context.Context, r RegisterRequest, assigned string, reserved, final bool) (registerPlan, string) {
	plan := registerPlan{name: assigned}
	plan.existing = s.durableIdentityLocked(r.Harness, r.SessionID, r.ClientID)
	if e := plan.existing; e != nil {
		if !secretMatches(e.record.SecretHash, r.Secret) {
			return plan, CodeOwned
		}
		if e.record.Suspended {
			return plan, CodeSuspended
		}
		if s.authorizeLocked(ctx, e) == nil && s.ackDowngradeBlocked(e, r.Capabilities) {
			return plan, CodeStorage
		}
		if e.record.LastRequestID == r.RequestID && e.record.LastRequestRevision == r.ExpectedRevision && (e.retired || (e.identity.ChannelID == r.ChannelID && (sameReceiverHolder(e.holder, r.Holder) || (e.holder == nil && r.Holder == nil)))) {
			if s.authorizeLocked(ctx, e) != nil {
				return plan, CodeOwned
			}
			plan.replay = true
			return plan, ""
		}
		if final && s.authorizeLocked(ctx, e) != nil {
			return plan, CodeOwned
		}
		if s.authorizeLocked(ctx, e) == nil && s.holderBlockedLocked(e, r.Holder, r.HolderProof, r.ChannelID) {
			return plan, protocol.CodeRegistrationInUse
		}
		// Explicit nickname changes use RenameSession. Re-registration keeps
		// the current name, including when an HTTP grant predates a rename.
		if grant, ok := access.FromContext(ctx); reserved && ok && grant.CheckName != nil {
			assigned, plan.name = e.record.Name, e.record.Name
		}
		if r.ExpectedRevision != e.record.Revision {
			return plan, CodeRevisionMismatch
		}
		// The owner re-registering on its still-live lease (the model ran the
		// register command again) keeps that lease; a retired lease is fenced.
		plan.sameLease = e.identity.ChannelID == r.ChannelID && !e.retired
		if _, used := e.leases[r.ChannelID]; !plan.sameLease && (used || e.identity.ChannelID == r.ChannelID) {
			return plan, CodeInvalid
		}
	} else if r.ExpectedRevision != 0 {
		return plan, CodeNotFound
	}
	if holder := s.sessions[assigned]; s.nameHolderBlocks(holder, plan.existing) {
		if reserved {
			return plan, CodeOwned
		}
		plan.requested = assigned
		plan.name = s.uniqueNicknameLocked()
		plan.warning = fmt.Sprintf("communication name %q is registered by another session; this session was assigned the UUID nickname %s", assigned, plan.name)
	}
	if plan.existing == nil {
		if len(s.sessions) >= remoteSessionCapacity {
			return plan, CodeCapacity
		}
		plan.registrationID = rand.Text()
		if final {
			// A grant revoked between Claim and commit must not create a live
			// registration; the check sees the session exactly as it would exist.
			session := Session{ID: r.SessionID, Name: plan.name, Harness: r.Harness, PID: r.PID, ExternallyManaged: true, Status: StatusAlive, RegistrationID: plan.registrationID, ClientID: r.ClientID, RequestedName: plan.requested, Revision: 1}
			if s.authorizeSessionLocked(ctx, session) != nil {
				return plan, CodeOwned
			}
		}
	}
	return plan, ""
}

func registrationWarning(code string) string {
	if code == CodeStorage {
		return ackDowngradeWarning
	}
	if code == CodeCapacity {
		return "remote session capacity reached; ask an administrator to detach old sessions before registering a new session"
	}
	return ""
}

func (s *RemoteService) RegisterSession(ctx context.Context, r RegisterRequest) (RegisterResult, error) {
	if err := ctx.Err(); err != nil {
		return RegisterResult{}, err
	}
	if !s.validateRegister(r) {
		return RegisterResult{Code: CodeInvalid}, nil
	}
	s.mu.Lock()
	admission, reserved := s.reservedLocked(ctx)
	s.mu.Unlock()
	assigned := r.Name
	var claimer claimingAdmission
	if reserved {
		name, code, err := admission.Admit(ctx, r)
		if err != nil {
			return RegisterResult{}, err
		}
		if code != "" {
			return RegisterResult{Code: code, Warning: registrationWarning(code)}, nil
		}
		if !validDurableName(name) {
			return RegisterResult{Code: CodeInvalid}, nil
		}
		assigned = name
		claimer, _ = admission.(claimingAdmission)
	} else if !validRemoteName(assigned) {
		return RegisterResult{Code: CodeInvalid}, nil
	}
	if claimer != nil {
		s.mu.Lock()
		s.sweepLocked()
		plan, code := s.planRegisterLocked(ctx, r, assigned, reserved, false)
		if code == "" && plan.replay {
			if err := s.updateReattachCapabilityLocked(plan.existing, r.Capabilities); err != nil {
				s.mu.Unlock()
				return RegisterResult{Code: CodeStorage}, nil
			}
			if plan.existing.identity.ChannelID == r.ChannelID {
				plan.existing.messageAck = plan.existing.messageAck || protocol.SupportsMessageAck(r.Capabilities)
				plan.existing.messageSentAt = protocol.SupportsMessageSentAt(r.Capabilities)
				plan.existing.messageAttachment = protocol.SupportsAttachment(r.Capabilities)
			}

			result := RegisterResult{MessageAckVersion: plan.existing.ackVersion(), RuntimeInfoVersion: protocol.RuntimeInfoVersion, ActivityVersion: protocol.ActivityVersion, Session: s.durableSnapshot(plan.existing), PendingProbe: s.pendingProbe(plan.existing)}
			s.mu.Unlock()
			return result, nil
		}
		s.mu.Unlock()
		if code != "" {
			if code == protocol.CodeRegistrationInUse {
				s.mu.Lock()
				defer s.mu.Unlock()
				if s.authorizeLocked(ctx, plan.existing) == nil {
					return s.holderResultLocked(plan.existing), nil
				}
				return RegisterResult{Code: CodeOwned}, nil
			}
			return RegisterResult{Code: code, Warning: registrationWarning(code)}, nil
		}
		code, err := claimer.Claim(ctx, r)
		if err != nil {
			return RegisterResult{}, err
		}
		if code != "" {
			return RegisterResult{Code: code, Warning: registrationWarning(code)}, nil
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	plan, code := s.planRegisterLocked(ctx, r, assigned, reserved, true)
	if code != "" {
		if code == protocol.CodeRegistrationInUse {
			return s.holderResultLocked(plan.existing), nil
		}
		return RegisterResult{Code: code, Warning: registrationWarning(code)}, nil
	}
	if plan.replay {
		if err := s.updateReattachCapabilityLocked(plan.existing, r.Capabilities); err != nil {
			return RegisterResult{Code: CodeStorage}, nil
		}
		if plan.existing.identity.ChannelID == r.ChannelID {
			plan.existing.messageAck = plan.existing.messageAck || protocol.SupportsMessageAck(r.Capabilities)
			plan.existing.messageSentAt = protocol.SupportsMessageSentAt(r.Capabilities)
			plan.existing.messageAttachment = protocol.SupportsAttachment(r.Capabilities)
		}

		return RegisterResult{MessageAckVersion: plan.existing.ackVersion(), RuntimeInfoVersion: protocol.RuntimeInfoVersion, ActivityVersion: protocol.ActivityVersion, Session: s.durableSnapshot(plan.existing), PendingProbe: s.pendingProbe(plan.existing)}, nil
	}
	if err := s.directFenceLocked(ctx, r.Harness, r.SessionID, r.ClientID, plan.registrationID, plan.name, plan.existing); err != nil {
		return RegisterResult{Code: CodeOwned}, nil
	}
	now := s.now()
	if plan.existing == nil {
		record := registrationRecord{
			OwnerID: s.registrationOwnerLocked(ctx), Version: registrationRecordVersion, RegistrationID: plan.registrationID, Harness: r.Harness, SessionID: r.SessionID, ClientID: r.ClientID,
			Name: plan.name, RequestedName: plan.requested, SecretHash: hashSecret(r.Secret), Revision: 1, LeaseGeneration: s.generation + 1,
			CreatedAt: now, LastSeen: now, PID: r.PID, CWD: r.CWD, ProcessKey: r.ProcessKey, MAC: r.MAC, LastRequestID: r.RequestID, LastRequestRevision: r.ExpectedRevision,
		}
		s.admissionMetadata(ctx, &record)
		record.ReattachAfterCleanup = slices.Contains(r.Capabilities, protocol.ReattachAfterCleanupCapability)
		if err := s.persistLocked(&record); err != nil {
			return RegisterResult{Code: CodeStorage}, nil
		}
		s.generation++
		if holder := s.sessions[plan.name]; holder != nil {
			delete(s.sessions, plan.name)
		}
		e := &remoteSession{
			messageSentAt: protocol.SupportsMessageSentAt(r.Capabilities),
			identity:      AttachRequest{Harness: r.Harness, Name: plan.name, SessionID: r.SessionID, PID: r.PID, CWD: r.CWD},
			leases:        map[string]struct{}{},
			changed:       make(chan struct{}),
			record:        &record,
			session:       Session{ID: r.SessionID, Name: plan.name, Harness: r.Harness, PID: r.PID, ExternallyManaged: true, StartedAt: now, Epoch: s.epoch},
		}
		assignReceiverHolder(e, r.Holder)
		s.issueLeaseLocked(e, r.ChannelID, now, r.Capabilities)
		e.session.AutomaticDeliveryReady = !r.IngressPending
		s.sessions[plan.name] = e
		s.byRegistration[record.RegistrationID] = e

		return RegisterResult{MessageAckVersion: e.ackVersion(), RuntimeInfoVersion: protocol.RuntimeInfoVersion, ActivityVersion: protocol.ActivityVersion, Session: s.durableSnapshot(e), Warning: plan.warning}, nil
	}
	existing := plan.existing
	next := *existing.record
	s.admissionMetadata(ctx, &next)
	next.ReattachAfterCleanup = slices.Contains(r.Capabilities, protocol.ReattachAfterCleanupCapability)
	if owner := s.registrationOwnerLocked(ctx); owner != "" {
		next.OwnerID = owner
	}
	next.Name, next.RequestedName = plan.name, plan.requested
	next.Revision++
	if !plan.sameLease {
		next.LeaseGeneration = s.generation + 1
	}
	next.Detached, next.DetachedAt = false, time.Time{}
	next.PID, next.CWD = r.PID, r.CWD
	if r.ProcessKey != "" {
		next.ProcessKey = r.ProcessKey
	}
	if r.MAC != "" {
		next.MAC = r.MAC
	}
	next.LastSeen = now
	next.LastRequestID, next.LastRequestRevision = r.RequestID, r.ExpectedRevision
	next.Probe = nil
	if err := s.persistLocked(&next); err != nil {
		return RegisterResult{Code: CodeStorage}, nil
	}
	if !plan.sameLease {
		s.generation++
	}
	if existing.record.Name != plan.name {
		if s.sessions[existing.record.Name] == existing {
			delete(s.sessions, existing.record.Name)
		}
		if holder := s.sessions[plan.name]; holder != nil && holder != existing {
			delete(s.sessions, plan.name)
		}
		s.sessions[plan.name] = existing
		existing.identity.Name = plan.name
		existing.session.Name = plan.name
	}
	*existing.record = next
	assignReceiverHolder(existing, r.Holder)
	existing.messageAck = existing.messageAck || protocol.SupportsMessageAck(r.Capabilities)
	existing.messageSentAt = protocol.SupportsMessageSentAt(r.Capabilities)
	existing.messageAttachment = protocol.SupportsAttachment(r.Capabilities)
	existing.identity.PID, existing.identity.CWD = r.PID, r.CWD
	existing.session.PID = r.PID
	if plan.sameLease {
		existing.clearActivity("evidence-missing")
		existing.until = now.Add(remoteLeaseDuration)
		if !r.IngressPending {
			existing.session.AutomaticDeliveryReady = true
		}

		return RegisterResult{MessageAckVersion: existing.ackVersion(), RuntimeInfoVersion: protocol.RuntimeInfoVersion, ActivityVersion: protocol.ActivityVersion, Session: s.durableSnapshot(existing), Warning: plan.warning, PendingProbe: s.pendingProbe(existing)}, nil
	}
	// Messages queued while the registration was alive stay queued for the
	// replacement receiver, including a bounded hold after ingress expiry.
	s.issueLeaseLocked(existing, r.ChannelID, now, r.Capabilities)
	existing.session.AutomaticDeliveryReady = !r.IngressPending

	return RegisterResult{MessageAckVersion: existing.ackVersion(), RuntimeInfoVersion: protocol.RuntimeInfoVersion, ActivityVersion: protocol.ActivityVersion, Session: s.durableSnapshot(existing), Warning: plan.warning}, nil
}

func (s *RemoteService) ResumeSession(ctx context.Context, r ResumeRequest) (RegisterResult, error) {
	if err := ctx.Err(); err != nil {
		return RegisterResult{}, err
	}
	if !registrationIDPattern.MatchString(r.RegistrationID) || (r.Harness != "claude" && r.Harness != "codex" && r.Harness != "kimi") || !validCanonicalConversationID(r.SessionID) ||
		r.PID <= 0 || !validRemoteCWD(r.CWD) || !clientIDPattern.MatchString(r.ClientID) || !validLeaseID(r.ChannelID) ||
		!printableToken(r.Secret, 32, 256) || !printableToken(r.RequestID, 1, 128) || len(r.ProcessKey) > 512 || !protocol.ValidReceiverHolder(r.Holder) {
		return RegisterResult{Code: CodeInvalid}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	now := s.now()
	e := s.byRegistration[r.RegistrationID]
	if e == nil {
		return RegisterResult{Code: CodeNotFound}, nil
	}
	if e.record.Harness != r.Harness || e.record.SessionID != r.SessionID || e.record.ClientID != r.ClientID || !secretMatches(e.record.SecretHash, r.Secret) || s.authorizeLocked(ctx, e) != nil {
		return RegisterResult{Code: CodeOwned}, nil
	}
	if e.record.Suspended {
		return RegisterResult{Code: CodeSuspended}, nil
	}
	if s.ackDowngradeBlocked(e, r.Capabilities) {
		return RegisterResult{Code: CodeStorage, Warning: ackDowngradeWarning}, nil
	}
	if e.record.LastRequestID == r.RequestID && e.record.LastRequestRevision == r.ExpectedRevision && (e.retired || (e.identity.ChannelID == r.ChannelID && (sameReceiverHolder(e.holder, r.Holder) || (e.holder == nil && r.Holder == nil)))) {
		if err := s.updateReattachCapabilityLocked(e, r.Capabilities); err != nil {
			return RegisterResult{Code: CodeStorage}, nil
		}
		if e.identity.ChannelID == r.ChannelID {
			e.messageAck = e.messageAck || protocol.SupportsMessageAck(r.Capabilities)
			e.messageSentAt = protocol.SupportsMessageSentAt(r.Capabilities)
			e.messageAttachment = protocol.SupportsAttachment(r.Capabilities)
		}

		return RegisterResult{MessageAckVersion: e.ackVersion(), RuntimeInfoVersion: protocol.RuntimeInfoVersion, ActivityVersion: protocol.ActivityVersion, Session: s.durableSnapshot(e), PendingProbe: s.pendingProbe(e)}, nil
	}
	if e.record.Detached {
		return RegisterResult{Code: CodeDetached}, nil
	}
	if s.holderBlockedLocked(e, r.Holder, r.HolderProof, r.ChannelID) {
		return s.holderResultLocked(e), nil
	}
	// The revision is mandatory: a resume delayed from before a restart must
	// never replace the lease a newer resume established afterwards.
	if r.ExpectedRevision != e.record.Revision {
		return RegisterResult{Code: CodeRevisionMismatch}, nil
	}
	if _, used := e.leases[r.ChannelID]; used || e.identity.ChannelID == r.ChannelID {
		return RegisterResult{Code: CodeInvalid}, nil
	}
	if err := s.directFenceLocked(ctx, r.Harness, r.SessionID, r.ClientID, r.RegistrationID, e.record.Name, e); err != nil {
		return RegisterResult{Code: CodeOwned}, nil
	}
	next := *e.record
	s.admissionMetadata(ctx, &next)
	next.ReattachAfterCleanup = slices.Contains(r.Capabilities, protocol.ReattachAfterCleanupCapability)
	if owner := s.registrationOwnerLocked(ctx); owner != "" {
		next.OwnerID = owner
	}
	next.Revision++
	next.LeaseGeneration = s.generation + 1
	next.PID, next.CWD = r.PID, r.CWD
	if r.ProcessKey != "" {
		next.ProcessKey = r.ProcessKey
	}
	next.LastSeen = now
	next.LastRequestID, next.LastRequestRevision = r.RequestID, r.ExpectedRevision
	if next.Probe != nil {
		probe := *next.Probe
		probe.Revision = next.Revision
		if !now.Before(s.effectiveDeadline(probe.Deadline)) {
			probe.ID, probe.IssuedAt, probe.Deadline = rand.Text(), now, now.Add(s.probeWindow)
		}
		next.Probe = &probe
	}
	if err := s.persistLocked(&next); err != nil {
		return RegisterResult{Code: CodeStorage}, nil
	}
	s.generation++
	*e.record = next
	assignReceiverHolder(e, r.Holder)
	e.messageAck = e.messageAck || protocol.SupportsMessageAck(r.Capabilities)
	e.messageSentAt = protocol.SupportsMessageSentAt(r.Capabilities)
	e.messageAttachment = protocol.SupportsAttachment(r.Capabilities)
	e.identity.PID, e.identity.CWD = r.PID, r.CWD
	e.session.PID = r.PID
	s.issueLeaseLocked(e, r.ChannelID, now, r.Capabilities)
	e.session.AutomaticDeliveryReady = !r.IngressPending

	return RegisterResult{MessageAckVersion: e.ackVersion(), RuntimeInfoVersion: protocol.RuntimeInfoVersion, ActivityVersion: protocol.ActivityVersion, Session: s.durableSnapshot(e), PendingProbe: s.pendingProbe(e)}, nil
}

func (s *RemoteService) RenewLease(ctx context.Context, r RenewRequest) (RegisterResult, error) {
	if err := ctx.Err(); err != nil {
		return RegisterResult{}, err
	}
	if !registrationIDPattern.MatchString(r.RegistrationID) || !validLeaseID(r.ChannelID) {
		return RegisterResult{Code: CodeInvalid}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	e := s.byRegistration[r.RegistrationID]
	if e == nil {
		return RegisterResult{Code: CodeNotFound}, nil
	}
	if s.authorizeLocked(ctx, e) != nil {
		return RegisterResult{Code: CodeOwned}, nil
	}
	if e.record.Suspended {
		return RegisterResult{Code: CodeSuspended}, nil
	}
	if e.record.Detached {
		return RegisterResult{Code: CodeDetached}, nil
	}
	if e.retired || e.identity.ChannelID != r.ChannelID {
		return RegisterResult{Code: CodeInvalid}, nil
	}
	n, ackErr := e.validateAck(r.Epoch, r.AckThrough, 0, false)
	if ackErr != nil {
		return RegisterResult{Code: CodeInvalid}, nil
	}
	s.applyAck(e, n)
	now := s.now()
	e.until = now.Add(remoteLeaseDuration)
	// Activation is monotonic within a lease: a delayed heartbeat sent before
	// activation cannot make an already activated receiver pending again.
	if !r.IngressPending {
		e.session.AutomaticDeliveryReady = true
	}
	s.applyActivityLocked(ctx, e, r.Activity, now)
	s.applyRuntimeInfoLocked(ctx, e, r.RuntimeInfo, now)
	e.record.LastSeen = now
	e.dirty = true

	return RegisterResult{MessageAckVersion: e.ackVersion(), RuntimeInfoVersion: protocol.RuntimeInfoVersion, ActivityVersion: protocol.ActivityVersion, Session: s.durableSnapshot(e), PendingProbe: s.pendingProbe(e)}, nil
}

func (s *RemoteService) AnswerProbe(ctx context.Context, a ProbeAnswer) (ProbeResult, error) {
	if err := ctx.Err(); err != nil {
		return ProbeResult{}, err
	}
	if !registrationIDPattern.MatchString(a.RegistrationID) || a.ProbeID == "" || len(a.ProbeID) > 128 || !validLeaseID(a.ChannelID) || len(a.CWD) > 4096 || len(a.ProcessKey) > 512 {
		return ProbeResult{Code: CodeInvalid}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	e := s.byRegistration[a.RegistrationID]
	if e == nil {
		return ProbeResult{Code: CodeNotFound}, nil
	}
	if s.authorizeLocked(ctx, e) != nil {
		return ProbeResult{Code: CodeOwned}, nil
	}
	if e.record.Suspended {
		return ProbeResult{Code: CodeSuspended}, nil
	}
	now := s.now()
	probe := e.record.Probe
	if probe == nil || probe.ID != a.ProbeID || probe.Revision != a.Revision || e.retired || e.identity.ChannelID != a.ChannelID || !now.Before(s.effectiveDeadline(probe.Deadline)) {
		return ProbeResult{Code: CodeStaleProbe}, nil
	}
	if !a.OK {
		next := *e.record
		next.Probe = nil
		if err := s.persistLocked(&next); err != nil {
			return ProbeResult{Code: CodeStorage}, nil
		}
		*e.record = next
		s.retire(e, StatusMissing, "identity-mismatch")
		return ProbeResult{Session: s.durableSnapshot(e)}, nil
	}
	next := *e.record
	next.Probe = nil
	next.LastSeen = now
	if a.PID > 0 {
		next.PID = a.PID
	}
	if a.CWD != "" && validRemoteCWD(a.CWD) {
		next.CWD = a.CWD
	}
	if a.ProcessKey != "" {
		next.ProcessKey = a.ProcessKey
	}
	if err := s.persistLocked(&next); err != nil {
		return ProbeResult{Code: CodeStorage}, nil
	}
	*e.record = next
	e.session.PID = next.PID
	return ProbeResult{Session: s.durableSnapshot(e)}, nil
}

// sweepLocked expires leases, issues probes to registrations missing for an
// hour, and detaches unanswered registrations after 30 days without contact.
// Suspended registrations remain; detached records use the existing removal path.
// Everything runs under the service lock, so a renewal or resume racing the
// sweep is ordered, never interleaved.
func (s *RemoteService) sweepLocked() {
	now := s.now()
	s.expireMissingLocked(now)
	var detached []*remoteSession
	for _, e := range s.sessions {
		s.expire(e)
	}
	for _, e := range s.byRegistration {
		s.expire(e)
		r := e.record
		if r.Suspended {
			continue
		}
		if r.Probe != nil {
			if !now.Before(s.effectiveDeadline(r.Probe.Deadline)) {
				s.retire(e, StatusMissing, "probe-unanswered")
				e.session.Status, e.session.ErrorCode = StatusMissing, "probe-unanswered"
			}
			continue
		}
		if !e.retired {
			continue
		}
		if r.Detached {
			if !now.Before(s.effectiveDeadline(r.DetachedAt.Add(s.probeAfter + s.probeWindow))) {
				detached = append(detached, e)
			}
			continue
		}
		if now.Before(r.LastSeen.Add(s.probeAfter)) {
			continue
		}
		r.Probe = &probeRecord{ID: rand.Text(), IssuedAt: now, Deadline: now.Add(s.probeWindow), Revision: r.Revision}
		_ = s.persistLocked(r)
	}
	_ = s.removeEntriesLocked(detached, "detached-expired")
	if !now.Before(s.lastFlush.Add(s.persistInterval)) {
		_ = s.flushLastSeenLocked()
	}
}

// Operation-triggered sweeps share the same budget as the background ticker.
func (s *RemoteService) expireMissingLocked(now time.Time) {
	if now.Before(s.lastExpire.Add(s.sweepInterval)) {
		return
	}
	s.lastExpire = now
	attempts := 0
	for _, e := range s.byRegistration {
		r := e.record
		if r.Suspended || r.Detached || r.Probe == nil || now.Before(s.effectiveDeadline(r.Probe.Deadline)) || now.Before(r.LastSeen.Add(missingRegistrationRetention)) {
			continue
		}
		attempts++
		if err := s.stopSessionLocked(e, nil); err != nil || attempts >= missingExpireBudget {
			return
		}
	}
}

func (s *RemoteService) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
}

func (s *RemoteService) run() {
	defer close(s.done)
	ticker := time.NewTicker(s.sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.sweep()
		}
	}
}

// Close stops background cleanup and releases blocked receivers. It attempts
// one dirty LastSeen flush before closing the store; Batch/fsync has no hard
// time bound. State access still works afterwards, with sweeps only on demand.
func (s *RemoteService) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		if s.stop != nil {
			close(s.stop)
			<-s.done
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = s.flushLastSeenLocked()
		if s.store != nil {
			_ = s.store.close()
		}
	})
	if s.store != nil {
		return s.store.closeErr
	}
	return nil
}

func (s *RemoteService) loadLocked() error {
	records, err := s.store.loadAll(s.now())
	if err != nil {
		return err
	}
	for i := range records {
		record := records[i]
		if holder := s.sessions[record.Name]; holder != nil {
			loser := &record
			if record.LastSeen.After(holder.record.LastSeen) {
				loser = holder.record
				delete(s.byRegistration, holder.record.RegistrationID)
			} else {
				s.store.quarantine(record.RegistrationID+".json", s.now())
				continue
			}
			s.store.quarantine(loser.RegistrationID+".json", s.now())
		}
		code := "server-restarted"
		if record.Detached {
			code = "detached"
		}
		status := StatusMissing
		if record.Suspended {
			status, code = StatusSuspended, CodeSuspended
		}
		e := &remoteSession{
			identity: AttachRequest{Harness: record.Harness, Name: record.Name, SessionID: record.SessionID, PID: record.PID, CWD: record.CWD},
			leases:   map[string]struct{}{},
			changed:  make(chan struct{}),
			retired:  true,
			record:   &record,
			session:  Session{ID: record.SessionID, Name: record.Name, Harness: record.Harness, PID: record.PID, ExternallyManaged: true, StartedAt: record.CreatedAt, Generation: record.LeaseGeneration, Status: status, ErrorCode: code, Epoch: s.epoch},
		}
		if record.LeaseGeneration > s.generation {
			s.generation = record.LeaseGeneration
		}
		if record.SuspendedLeaseID != "" {
			s.fenceLease(e, record.SuspendedLeaseID)
		}
		s.sessions[record.Name] = e
		s.byRegistration[record.RegistrationID] = e
	}
	return nil
}

func (s *RemoteService) updateReattachCapabilityLocked(e *remoteSession, capabilities []string) error {
	enabled := slices.Contains(capabilities, protocol.ReattachAfterCleanupCapability)
	if e.record.ReattachAfterCleanup == enabled {
		return nil
	}
	next := *e.record
	next.ReattachAfterCleanup = enabled
	if err := s.persistLocked(&next); err != nil {
		return err
	}
	*e.record = next
	return nil
}
