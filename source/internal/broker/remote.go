package broker

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/attachments"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"path"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"

	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const remoteLeaseDuration = 30 * time.Second
const remoteSessionCapacity = 1024
const remoteLeaseHistoryCapacity = 1024

// Accepted messages wait only in memory for the same durable registration.
const expiredIngressQueueRetention = 10 * time.Minute

// RemoteService trusts local MCP relays to observe their own agents. It never
// interprets a reported PID or cwd in the central server's process/filesystem.
// Lease identifiers separate receivers; they are not user authentication.
// Durable registrations (see remote_durable.go) persist only the name→identity
// binding; liveness always comes from a current lease.
type RemoteService struct {
	attachments          *attachments.Store
	detachedCleanupBatch func([]access.DetachedRemovalProof) error

	mu             sync.Mutex
	sessions       map[string]*remoteSession
	byRegistration map[string]*remoteSession
	generation     uint64
	now            func() time.Time
	epoch          string
	store          *registrationStore
	admission      Admission

	bootedAt        time.Time
	probeAfter      time.Duration
	probeWindow     time.Duration
	bootGrace       time.Duration
	sweepInterval   time.Duration
	persistInterval time.Duration
	lastFlush       time.Time
	lastExpire      time.Time
	stop            chan struct{}
	done            chan struct{}
	closed          chan struct{}
	closeOnce       sync.Once
}
type remoteSession struct {
	messageAttachment     bool
	attachments           *attachments.Store
	observedPeers         map[string]observedPeer
	peerSequence          uint64
	messageAck            bool
	inflight              []Message
	inflightBytes         int
	unacked               map[uint64]bool
	ackProtectedUntil     time.Time
	issuedThrough         uint64
	leaseAckThrough       uint64
	ackedThrough          uint64
	holder                *protocol.ReceiverHolder
	holderChallenge       *protocol.HolderChallenge
	holderChallengeUntil  time.Time
	runtimeInfo           *protocol.RuntimeInfoSnapshot
	runtimeSequence       uint64
	runtimeObservedAt     time.Time
	runtimeAuthorization  func() error
	activity              *protocol.ActivitySnapshot
	activitySequence      uint64
	activityObservedAt    time.Time
	activityAuthorization func() error
	messageSentAt         bool
	identity              AttachRequest
	session               Session
	until                 time.Time
	retired               bool
	leases                map[string]struct{}
	leaseOrder            []string
	queue                 []Message
	queueHeldUntil        time.Time
	changed               chan struct{}
	record                *registrationRecord
	dirty                 bool
}

// NewRemoteService keeps every registration in memory only.
func NewRemoteService() *RemoteService {
	s, _ := newRemoteService("", false)
	return s
}

// NewPersistentRemoteService restores durable registrations from root and
// writes every binding change through to it. Restored registrations are
// missing until their owner resumes with the secret and a fresh lease.
func NewPersistentRemoteService(root string) (*RemoteService, error) {
	return newRemoteService(root, true)
}

func newRemoteService(root string, background bool) (*RemoteService, error) {
	return newRemoteServiceWithStore(root, background, false)
}

func newRemoteServiceWithStore(root string, background, sqlite bool) (*RemoteService, error) {
	return newRemoteServiceWithCleanup(root, background, sqlite, nil)
}

func newRemoteServiceWithCleanup(root string, background, sqlite bool, batch func([]access.DetachedRemovalProof) error) (*RemoteService, error) {
	now := time.Now()
	s := &RemoteService{
		sessions: map[string]*remoteSession{}, byRegistration: map[string]*remoteSession{}, now: time.Now, epoch: rand.Text(),
		bootedAt: now, probeAfter: defaultProbeAfter, probeWindow: defaultProbeWindow, bootGrace: defaultBootGrace,
		sweepInterval: defaultSweepInterval, persistInterval: defaultPersistInterval, lastFlush: now, lastExpire: now, closed: make(chan struct{}),
	}
	s.detachedCleanupBatch = batch
	if root != "" {
		store, err := newRegistrationStore(root)
		if err != nil {
			return nil, err
		}
		if sqlite {
			if err := store.openSQLite(now); err != nil {
				return nil, errors.Join(err, store.close())
			}
		}
		s.store = store
		if err := s.loadLocked(); err != nil {
			return nil, errors.Join(err, store.close())
		}
	}
	if background {
		s.stop = make(chan struct{})
		s.done = make(chan struct{})
		serverdiag.StartBackground(serverdiag.BackgroundStop, s.stop, s.run, nil)
	}
	return s, nil
}

var remoteWindowsAbsolute = regexp.MustCompile(`^[a-zA-Z]:[\\/]`)

// validRemoteName accepts user-requested names. Session UUIDs and registration
// ids are separate selector namespaces and can never be chosen as names.
func validRemoteName(name string) bool {
	return !protocol.IsAdminSender(name) && name != "" && len(name) <= 128 && !validCanonicalConversationID(name) && !registrationIDPattern.MatchString(name) && !typedSelector(name) && strings.TrimSpace(name) == name && strings.IndexFunc(name, invalidRemoteNameRune) < 0
}
func (s *RemoteService) AttachSession(ctx context.Context, r AttachRequest) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	if !validRemoteName(r.Name) || !validCanonicalConversationID(r.SessionID) || r.PID <= 0 || len(r.CWD) > 4096 || strings.ContainsRune(r.CWD, 0) || !(path.IsAbs(r.CWD) || remoteWindowsAbsolute.MatchString(r.CWD) || strings.HasPrefix(r.CWD, `\\`)) || (r.Harness != "codex" && r.Harness != "claude" && r.Harness != "kimi") || r.ChannelID == "" || len(r.ChannelID) > 128 {
		return Session{}, protocol.SafeAttachRejection("invalid-identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, reserved := s.reservedLocked(ctx); reserved {
		// Token-protected requests bind names through register_session only;
		// the legacy explicit-identity attach would bypass ownership checks.
		return Session{}, protocol.SafeAttachRejection("legacy-disabled")
	}
	s.sweepLocked()
	var previous *remoteSession
	for _, entry := range s.sessions {
		if entry.record != nil {
			continue
		}
		if entry.identity.SessionID == r.SessionID {
			previous = entry
			if entry.identity.Name != r.Name && !entry.retired {
				return Session{}, protocol.SafeAttachRejection("identity-conflict")
			}
			if _, used := entry.leases[r.ChannelID]; used && (entry.retired || entry.identity.ChannelID != r.ChannelID) {
				return Session{}, protocol.SafeAttachRejection("lease-conflict")
			}
		}
	}
	if old := s.sessions[r.Name]; old != nil {
		if old.record != nil {
			return Session{}, protocol.SafeAttachRejection("name-in-use")
		}
		if old.identity.ChannelID == r.ChannelID {
			if old.retired || old.identity.Harness != r.Harness || old.identity.Name != r.Name || old.identity.SessionID != r.SessionID || old.identity.PID != r.PID || old.identity.CWD != r.CWD {
				return Session{}, protocol.SafeAttachRejection("lease-conflict")
			}
			old.messageSentAt = protocol.SupportsMessageSentAt(r.Capabilities)
			old.messageAttachment = protocol.SupportsAttachment(r.Capabilities)
			old.until = s.now().Add(remoteLeaseDuration)
			s.notify(old)
			return s.snapshot(old), nil
		}
		if !old.retired {
			return Session{}, protocol.SafeAttachRejection("name-in-use")
		}
	} else if previous == nil && len(s.sessions) >= remoteSessionCapacity {
		return Session{}, protocol.SafeAttachRejection("capacity")
	}
	leases := map[string]struct{}{}
	if previous != nil {
		// Keep old leases fenced even after a renamed registration is detached.
		// A delayed heartbeat must never restore a previously retired name.
		leases = previous.leases
		if len(leases) >= remoteLeaseHistoryCapacity {
			return Session{}, protocol.SafeAttachRejection("capacity")
		}
	}
	leases[r.ChannelID] = struct{}{}
	s.generation++
	entry := &remoteSession{messageAttachment: protocol.SupportsAttachment(r.Capabilities), attachments: s.attachments, messageSentAt: protocol.SupportsMessageSentAt(r.Capabilities), identity: r, leases: leases, until: s.now().Add(remoteLeaseDuration), changed: make(chan struct{}), session: Session{ID: r.SessionID, Name: r.Name, Harness: r.Harness, PID: r.PID, ExternallyManaged: true, AutomaticDeliveryReady: true, Status: StatusAlive, Generation: s.generation, StartedAt: s.now(), Epoch: s.epoch}}
	if previous != nil && previous.identity.Name != r.Name {
		delete(s.sessions, previous.identity.Name)
	}
	s.sessions[r.Name] = entry
	return s.snapshot(entry), nil
}
func (s *RemoteService) notify(e *remoteSession) { close(e.changed); e.changed = make(chan struct{}) }
func (s *RemoteService) retire(e *remoteSession, status SessionStatus, code string) {
	// A terminal transition must also discard a queue held by an already
	// expired receiver. Legacy entries have no durable owner to resume it.
	if e.record != nil && (code == "ingress-expired" || (code == "ingress-failed" && e.messageAck)) {
		at := s.now()
		if code == "ingress-expired" {
			at = e.until
		}
		e.protectAckLoss(at)
		e.requeueInflight()
		if !e.retired && len(e.queue) != 0 {
			e.queueHeldUntil = at.Add(expiredIngressQueueRetention)
		}
	} else {
		e.discardMessages()
	}
	if e.retired {
		return
	}
	e.retired = true
	e.clearActivity("lease-inactive")
	e.session.Status = status
	e.session.ErrorCode = code
	e.session.AutomaticDeliveryReady = false
	s.notify(e)
}
func (s *RemoteService) expire(e *remoteSession) {
	if !e.retired && !s.now().Before(e.until) {
		s.retire(e, StatusMissing, "ingress-expired")
	}
	if !e.queueHeldUntil.IsZero() && !s.now().Before(e.queueHeldUntil) {
		if e.releaseAckProtection(s.now()) {
			s.notify(e)
			return
		}
		e.discardMessages()
		s.notify(e)
	}
}
func (s *RemoteService) snapshot(e *remoteSession) Session {
	v := e.session
	v.Activity = s.activitySnapshot(e)
	v.PendingMessages = len(e.queue) + len(e.inflight)
	if r := e.record; r != nil {
		v.RegistrationID = r.RegistrationID
		v.HandoverFrom = r.HandoverFrom
		if m := r.LastManagement; m != nil {
			v.LastManagement = &protocol.LastManagement{Kind: m.Kind, ActorName: m.ActorName, At: m.At}
		}
		v.Suspended = r.Suspended
		v.ClientID = r.ClientID
		v.Revision = r.Revision
		v.RequestedName = r.RequestedName
		v.Name = r.Name
		v.PID = r.PID
		if r.Probe != nil {
			v.ProbePending = true
			v.ProbeDeadline = s.effectiveDeadline(r.Probe.Deadline)
		}
	}
	return v
}

// Typed selector prefixes address exactly one namespace. Names can never start
// with one of them, so a bare selector stays backward compatible.
const (
	SelectorRegistration = protocol.SelectorRegistration
	SelectorName         = protocol.SelectorName
	SelectorSession      = protocol.SelectorSession
)

func typedSelector(value string) bool {
	return strings.HasPrefix(value, SelectorRegistration) || strings.HasPrefix(value, SelectorName) || strings.HasPrefix(value, SelectorSession)
}

// find resolves typed selectors first. A bare selector tries a registration id
// (a namespace no name can enter), then an exact communication name, then a
// session UUID. A UUID shared by registrations from different client ids, or
// equal to another session's nickname, is addressed with session: or name:.
func (s *RemoteService) find(selector string) *remoteSession {
	if rest, ok := strings.CutPrefix(selector, SelectorRegistration); ok {
		return s.byRegistration[rest]
	}
	if rest, ok := strings.CutPrefix(selector, SelectorName); ok {
		return s.sessions[rest]
	}
	if rest, ok := strings.CutPrefix(selector, SelectorSession); ok {
		return s.uniqueSession(rest)
	}
	if e := s.byRegistration[selector]; e != nil {
		return e
	}
	if e := s.sessions[selector]; e != nil {
		return e
	}
	return s.uniqueSession(selector)
}

func (s *RemoteService) uniqueSession(id string) *remoteSession {
	var match *remoteSession
	for _, e := range s.sessions {
		if e.session.ID == id {
			if match != nil {
				return nil
			}
			match = e
		}
	}
	return match
}
func (s *RemoteService) ListSessions(ctx context.Context) ([]Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	out := []Session{}
	for _, e := range s.sessions {
		out = append(out, s.snapshot(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
func (s *RemoteService) GetSession(ctx context.Context, name string) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	e := s.find(name)
	if e == nil {
		return Session{}, ErrSessionNotFound
	}
	return s.snapshot(e), nil
}
func (s *RemoteService) SendMessage(ctx context.Context, m Message) (SendResult, error) {
	acceptedAttachment := false
	var attachmentBindingSequence uint64
	if m.Attachment != nil {
		id, from := m.Attachment.ID, m.From
		defer func() {
			if !acceptedAttachment {
				s.deleteRejectedAttachment(ctx, from, id, attachmentBindingSequence)
			}
		}()
	}

	if !protocol.ValidRecipientExpectation(m.ExpectRegistrationID) {
		return SendResult{Code: protocol.CodeInvalid}, nil
	}
	if protocol.IsAdminSender(m.From) {
		return SendResult{}, errors.New("reserved system sender cannot be used by a session")
	}
	if protocol.IsAdminSender(m.To) {
		return SendResult{}, ErrSessionNotFound
	}
	if err := ctx.Err(); err != nil {
		return SendResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	from, to := s.find(m.From), s.find(m.To)
	if from == nil {
		return SendResult{}, ErrSessionNotFound
	}
	if protocol.IsAdminSender(from.session.Name) {
		return SendResult{}, errors.New("reserved system sender cannot be used by a session")
	}
	if err := s.authorizeLocked(ctx, from); err != nil {
		return SendResult{}, err
	}
	if to == nil {
		if m.ExpectRegistrationID != nil {
			return SendResult{Code: protocol.CodeRecipientChanged}, nil
		}
		return SendResult{}, ErrSessionNotFound
	}
	if protocol.IsAdminSender(to.session.Name) {
		return SendResult{}, ErrSessionNotFound
	}
	if m.ExpectRegistrationID != nil && (to.record == nil || to.record.RegistrationID != *m.ExpectRegistrationID) {
		result := SendResult{Code: protocol.CodeRecipientChanged}
		if to.record != nil && access.RequireScope(ctx, access.SessionRead) == nil {
			result.CurrentRegistrationID = to.record.RegistrationID
		}
		return result, nil
	}
	if s.staleNameLocked(ctx, from, to, m) {
		result := SendResult{Code: protocol.CodeRecipientChanged, Message: staleNameMessage(m.To)}
		if to.record != nil {
			result.CurrentRegistrationID = to.record.RegistrationID
		}
		return result, nil
	}
	if suspendedSession(from) || suspendedSession(to) {
		return SendResult{}, ErrSessionSuspended
	}
	s.expire(from)
	s.expire(to)
	if from.retired || to.retired {
		return SendResult{}, errors.New("sender and recipient must have live remote ingress leases")
	}
	// A send-only credential has no ingress to activate. Authorization above
	// still binds it to this live sender; recipients always need ready ingress.
	requireSenderReady := true
	if grant, ok := access.FromContext(ctx); ok && access.HasScope(grant.Scopes, access.MessageSend) && !access.HasScope(grant.Scopes, access.MessageReceive) {
		requireSenderReady = false
	}
	if (requireSenderReady && !from.session.AutomaticDeliveryReady) || !to.session.AutomaticDeliveryReady {
		return SendResult{}, errors.New("sender and recipient must have activated their delivery channels")
	}
	m.FromRegistrationID = ""
	if from.record != nil {
		m.FromRegistrationID = from.record.RegistrationID
	}
	confirmed := m.ExpectRegistrationID != nil
	m.ExpectRegistrationID = nil
	m.From = from.session.Name
	m.To = to.session.Name
	m.Batch = nil
	if m.Attachment != nil {
		if s.attachments == nil {
			return SendResult{Code: "attachments-disabled"}, nil
		}
		if !to.messageAttachment || !to.messageAck || to.session.Harness == "kimi" {
			return SendResult{Code: "attachment-unsupported"}, nil
		}
		owner, err := s.attachmentOwnerLocked(ctx, from.session.Name)
		if err != nil {
			return SendResult{}, err
		}
		ref, err := s.attachments.Bind(owner, m.Attachment.ID, to.record.RegistrationID, to.session.DeliverySequence+1)
		if err != nil {
			if e, ok := err.(*attachments.Error); ok && e.Code == "attachments-disabled" {
				return SendResult{Code: e.Code}, nil
			}
			return SendResult{Code: "attachment-failed"}, nil
		}
		attachmentBindingSequence = to.session.DeliverySequence + 1
		m.Attachment = ref
	}
	result, err := s.enqueueLocked(to, m)
	acceptedAttachment = result.Accepted

	if result.Accepted && to.record != nil {
		s.observePeerLocked(ctx, from, to.session.Name, to.record.RegistrationID, confirmed)
	}
	return result, err
}

func (s *RemoteService) enqueueLocked(to *remoteSession, m Message) (SendResult, error) {
	if protocol.IsAdminSender(m.From) {
		m.FromRegistrationID = ""
	}
	m.ExpectRegistrationID = nil
	m.DeliverySequence = 0
	m.SentAt = s.now().UTC().Truncate(time.Millisecond)
	// Validate exactly the routing fields the receiver will get, not caller input.
	m.RegistrationID = ""
	if to.record != nil {
		m.RegistrationID = to.record.RegistrationID
	}
	if to.messageAck {
		m.DeliverySequence = to.session.DeliverySequence + 1
	}
	data, err := json.Marshal(m)
	limit := protocol.MaxMessageBytes
	if m.Attachment != nil {
		limit -= protocol.AttachmentReserveBytes
	}
	if err != nil || m.Body == "" || len(data) > limit {
		return SendResult{}, errors.New("message requires a body and an envelope of at most 12000 bytes")
	}
	if len(to.queue) >= 32 {
		return SendResult{}, ErrQueueFull
	}
	to.attachments = s.attachments
	to.session.DeliverySequence++
	m.DeliverySequence = to.session.DeliverySequence
	to.queue = append(to.queue, m)
	s.notify(to)
	result := SendResult{OK: true, Accepted: true, SessionID: to.session.ID, DeliverySequence: to.session.DeliverySequence, WakeStatus: WakeNotNeeded}
	if to.session.Harness == "kimi" {
		result.WakeStatus = WakeDegraded
		result.WarningMessage = "queued for the next eligible UserPromptSubmit or Stop hook; idle, failed or continued turns may wait until the next user prompt"
	}
	return result, nil
}
func (s *RemoteService) WaitForMessage(context.Context, string) (Message, error) {
	return Message{}, errors.New("remote receiver requires its ingress lease; use the local MCP client")
}
func (s *RemoteService) WaitForIngress(ctx context.Context, name, lease string) (Message, error) {
	return s.waitForIngress(ctx, protocol.IngressRequest{Recipient: name, ChannelID: lease}, false)
}
func (s *RemoteService) waitForIngress(ctx context.Context, r protocol.IngressRequest, wire bool) (Message, error) {
	name, lease := r.Recipient, r.ChannelID
	s.mu.Lock()
	entry := s.find(name)
	s.mu.Unlock()
	if entry == nil {
		return Message{}, ErrSessionNotFound
	}
	for {
		if err := ctx.Err(); err != nil {
			return Message{}, err
		}
		select {
		case <-s.closed:
			return Message{}, errors.New("remote service is shutting down")
		default:
		}
		s.mu.Lock()
		s.expire(entry)
		if suspendedSession(entry) {
			s.mu.Unlock()
			return Message{}, ErrSessionSuspended
		}
		if entry.retired || lease == "" || entry.identity.ChannelID != lease {
			s.mu.Unlock()
			return Message{}, errors.New("remote ingress lease is no longer active")
		}
		if err := s.authorizeLocked(ctx, entry); err != nil {
			s.mu.Unlock()
			return Message{}, err
		}
		if !entry.session.AutomaticDeliveryReady {
			s.mu.Unlock()
			return Message{}, errors.New("remote delivery channel is not activated")
		}
		cursor := r.Cursor
		if !wire {
			cursor = entry.issuedThrough
		}
		if entry.messageAck && wire && (r.RegistrationID != entry.record.RegistrationID || r.Epoch != entry.session.Epoch) {
			s.mu.Unlock()
			return Message{}, errors.New("message acknowledgement requires the current registration and epoch")
		}
		if r.RegistrationID != "" && (entry.record == nil || r.RegistrationID != entry.record.RegistrationID) {
			s.mu.Unlock()
			return Message{}, errors.New("registration mismatch")
		}
		n, err := entry.validateAck(r.Epoch, r.AckThrough, cursor, true)
		if err != nil {
			s.mu.Unlock()
			return Message{}, err
		}
		s.applyAck(entry, n)
		var m Message
		var ready bool
		if entry.messageAck {
			m, ready = entry.nextAckMessage(cursor)
		} else if len(entry.queue) > 0 {
			m = entry.queue[0]
			entry.queue[0] = Message{}
			entry.queue = entry.queue[1:]
			m.DeliverySequence = 0
			ready = true
		}
		if ready {
			s.observePeerLocked(ctx, entry, m.From, m.FromRegistrationID, false)
			if m.Batch != nil {
				for _, item := range m.Batch.Messages {
					s.observePeerLocked(ctx, entry, item.From, item.FromRegistrationID, false)
				}
			}
			if entry.record != nil {
				m.RegistrationID = entry.record.RegistrationID
			}
			m = projectAttachment(m, entry.messageAttachment, s.attachments != nil && s.attachments.Enabled())
			sentAt := entry.messageSentAt
			s.mu.Unlock()
			return protocol.ProjectMessage(m, sentAt), nil
		}
		changed := entry.changed
		remaining := entry.until.Sub(s.now())
		s.mu.Unlock()
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Message{}, ctx.Err()
		case <-s.closed:
			timer.Stop()
			return Message{}, errors.New("remote service is shutting down")
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}
func (s *RemoteService) ReportIngressFailure(ctx context.Context, id, lease string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.find(id)
	if e == nil || lease == "" || e.identity.ChannelID != lease {
		return errors.New("remote ingress lease mismatch")
	}
	if err := s.authorizeLocked(ctx, e); err != nil {
		return err
	}
	if suspendedSession(e) {
		return ErrSessionSuspended
	}
	s.retire(e, StatusDegraded, "ingress-failed")
	return nil
}
func (s *RemoteService) StopSession(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.find(name)
	if e == nil {
		return ErrSessionNotFound
	}
	if err := s.authorizeLocked(ctx, e); err != nil {
		return err
	}
	return s.stopSessionLocked(e, nil)
}
func (s *RemoteService) stopSessionLocked(e *remoteSession, management *protocol.ManagementEvent) error {
	if suspendedSession(e) {
		return ErrSessionSuspended
	}
	if e.record != nil {
		if e.record.Detached {
			return nil
		}
		next := *e.record
		next.Detached, next.DetachedAt = true, s.now()
		if management != nil {
			next.LastManagement = management
		}
		next.Probe = nil
		if err := s.persistLocked(&next); err != nil {
			return errors.New("detach could not be recorded")
		}
		*e.record = next
		e.discardMessages()
		if e.retired {
			e.session.Status, e.session.ErrorCode = StatusMissing, "detached"
			return nil
		}
	}
	if e.retired {
		e.session.Status, e.session.ErrorCode = StatusMissing, CodeDetached
		return nil
	}
	s.retire(e, StatusMissing, "detached")
	return nil
}

var ErrQueueFull = errors.New("message queue is full")

func validCanonicalConversationID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
