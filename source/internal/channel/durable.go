package channel

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/localactivity"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// durableOptions turns the local ingress client into an owner of durable
// registrations: it keeps a per-registration secret, resumes automatically
// after a network or server restart, and answers identity probes without the
// model. Everything here is process-local; nothing reaches the model.
type durableOptions struct {
	attachmentCache       string
	explicitAttachBarrier func(context.Context, string)
	codexSteerOff         bool
	codexClaim            *codexResumeClaim
	hookWrites            *hookWriteCompletions
	kimi                  *kimiClient
	identityFailure       error
	parentDiagnostic      *parentDiagnostic
	harness               string // Cheap ancestry classification; discovery never probes identity.
	claudeInitial         func(context.Context) (broker.AttachRequest, error)
	receiverIdentity      interface {
		Current(context.Context) *broker.ReceiverHolder
		Dead(context.Context, broker.HolderChallenge) bool
	}
	receive         *receiveScopes
	enabled         bool
	activityPool    *activityWorkers
	activityFactory func(localactivity.Config) activityObserver
	// tokenMode means an admission token is presented; the broker assigns the
	// nickname, so the model may register without a name.
	tokenMode bool
	state     *clientstate.Store
	// scope separates state and locks per broker endpoint and project so the
	// same session registered with two brokers never shares a secret.
	scope   string
	project string
	// observe re-verifies that the bound parent session still runs: for Claude
	// it re-reads the interactive roster, for Codex it re-checks the bound
	// thread under the bound PID.
	observe           func(ctx context.Context, bound broker.AttachRequest) (broker.AttachRequest, error)
	heartbeat         time.Duration
	timeout           time.Duration
	backoff           func(attempt int) time.Duration
	resumeClock       devicePollClock
	runProgress       *atomic.Bool
	parentProcessKey  func(context.Context, int) (string, error)
	claimProcessStart func(int) (time.Time, error)
	// serverOptions, configure and bound let a channel-mode server add the
	// Claude channel capability, its activation tools and learn the bound name.
	serverOptions func(*mcp.ServerOptions)
	configure     func(*mcp.Server)
	bound         func(broker.Session)
	// ready is closed only after a legacy Claude channel acknowledges activation.
	// Native ingress has no additional activation gate.
	ready <-chan struct{}
	// device switches the client to project-token mode (see deviceflow.go).
	device *deviceAuthOptions
	// connected reports a conversation's own upstream once its credential is
	// in use, so a channel-mode reply tool can send through it.
	connected func(Upstream)
	// bindingCreated observes creation for parent identity tracking and diagnostics.
	// The callback runs under b.mu: retain the binding without reading/locking
	// it, then take b.mu for later snapshots. Never publish identifiers/secrets.
	bindingCreated func(*ingressBinding)
	// claudeSessionFile injects the bounded process roster file reader.
	claudeSessionFile func(int) ([]byte, error)
	// Clock injection keeps bounded startup observations deterministic in tests.
	claudeFollowClock  devicePollClock
	claudeStartupClock devicePollClock
	claudeInboxFactory ingressSinkFactory
	claudeFollow       *claudeFollow
}

func (o durableOptions) ingressPending() bool {
	if o.ready == nil {
		return false
	}
	select {
	case <-o.ready:
		return false
	default:
		return true
	}
}

func (o durableOptions) withDefaults() durableOptions {
	if o.receive == nil {
		o.receive = &receiveScopes{}
	}
	if o.scope == "" {
		o.scope = o.project
	}
	if o.heartbeat <= 0 {
		o.heartbeat = 10 * time.Second
	}
	if o.timeout <= 0 {
		o.timeout = 5 * time.Second
	}
	if o.resumeClock.sleep == nil {
		o.resumeClock = realPollClock()
	}
	if o.backoff == nil {
		o.backoff = func(attempt int) time.Duration {
			delay := time.Second << uint(min(attempt, 4))
			if delay > 10*time.Second {
				delay = 10 * time.Second
			}
			return time.Duration(float64(delay) * (0.8 + mathrand.Float64()*0.4))
		}
	}
	return o
}

type durableRegistration struct {
	lastSessionTouch time.Time
	parentPID        int
	processKey       string
	handedOver       bool
	handoverFrom     *broker.HandoverFrom
	detached         bool
	initialAttempt   string
	key              string
	registrationID   string
	name             string
	secret           string
	clientID         string
	revision         uint64
	pendingRequest   string
	pendingLease     string
	pendingRevision  uint64
	// session is set in project-token mode: the registration fields above are
	// stored inside the conversation's SessionState instead of a legacy file.
	session *sessionAuth
}

type receiverExit int

const (
	exitContext receiverExit = iota
	exitUpstream
	exitSink
	exitTerminal
	exitScopeDenied
	exitFollow
)

func hasTool(tools []*mcp.Tool, name string) bool {
	for _, tool := range tools {
		if tool != nil && tool.Name == name {
			return true
		}
	}
	return false
}

// loadRegistration takes the registration lock for this session and loads or
// creates the local secret. The lock stays held for the binding's lifetime so
// a second local client cannot run a competing receiver.
func (o durableOptions) loadRegistration(identity broker.AttachRequest) (*durableRegistration, func(), error) {
	if o.device != nil {
		return o.loadSessionRegistration(identity)
	}
	key := clientstate.RegistrationKey(o.scope, identity.Harness, identity.SessionID)
	unlock, err := o.state.Lock(key)
	if err != nil {
		return nil, nil, err
	}
	clientID, err := o.state.ClientID()
	if err != nil {
		unlock()
		return nil, nil, err
	}
	saved, found, err := o.state.LoadRegistration(key)
	if err != nil {
		unlock()
		return nil, nil, err
	}
	d := &durableRegistration{key: key, clientID: clientID, parentPID: identity.PID, processKey: o.handoverProcessKey(identity)}
	if found {
		d.registrationID, d.name, d.secret, d.revision = saved.RegistrationID, saved.Name, saved.Secret, saved.Revision
		d.pendingRequest, d.pendingLease, d.pendingRevision = saved.PendingRequestID, saved.PendingLease, saved.PendingRevision
		d.initialAttempt = saved.InitialAttempt
		d.detached = saved.Detached
		d.handoverFrom = saved.HandoverFrom
		d.handedOver = saved.HandedOverTo != ""
		if d.initialAttempt == "" && d.registrationID == "" && d.pendingRequest != "" && d.pendingLease != "" {
			d.initialAttempt = initialPending
		}
	} else {
		d.secret = clientstate.NewSecret()
	}
	return d, unlock, nil
}

func (o durableOptions) save(identity broker.AttachRequest, d *durableRegistration) error {
	if d.session != nil {
		return o.saveSessionRegistration(identity, d)
	}
	return o.state.SaveRegistration(d.key, registrationSnapshot(d, identity, o.project))
}

func sameCWD(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// registerDurable performs the model's explicit registration. The request id,
// lease and expected revision are saved before the call so a lost answer is
// recovered by retrying the identical request, even from a new client
// process. A stale local state is repaired once: a removed registration is
// re-created with a fresh revision baseline and a newer server revision is
// re-read before retrying.
func registerDurable(ctx context.Context, upstream Upstream, b *ingressBinding, opts durableOptions, identity broker.AttachRequest) (broker.Session, error) {
	return registerDurableWithPolicy(ctx, upstream, b, opts, identity, true)
}

var errAutomaticStopped = errors.New("automatic registration stopped; explicit registration is required")

func registerDurableWithPolicy(ctx context.Context, upstream Upstream, b *ingressBinding, opts durableOptions, identity broker.AttachRequest, repair bool) (broker.Session, error) {
	b.registerMu.Lock()
	defer b.registerMu.Unlock()
	if repair {
		// Explicit registration supersedes any canceled automatic resume.
		b.pendingResume = broker.ResumeRequest{}
	}
	b.mu.Lock()
	d := b.durable
	b.mu.Unlock()
	if d == nil {
		return broker.Session{}, errors.New("durable registration state is unavailable")
	}
	request := broker.RegisterRequest{
		Capabilities: opts.deliveryCapabilities(ctx, upstream, identity.Harness),
		Harness:      identity.Harness, SessionID: identity.SessionID, PID: identity.PID, CWD: identity.CWD,
		ClientID: d.clientID, Name: identity.Name, ChannelID: identity.ChannelID, Secret: d.secret,
		MAC:            clientstate.MACAddress(),
		IngressPending: opts.bindingIngressPending(b, upstream),
	}
	if d.registrationID != "" {
		request.ExpectedRevision = d.revision
	}
	repaired := map[string]bool{}
	for {
		b.mu.Lock()
		if d.pendingRequest == "" || d.pendingLease != identity.ChannelID || d.pendingRevision != request.ExpectedRevision {
			d.pendingRequest, d.pendingLease, d.pendingRevision = rand.Text(), identity.ChannelID, request.ExpectedRevision
		}
		request.RequestID = d.pendingRequest
		saved := *d
		b.mu.Unlock()
		if err := opts.save(identity, &saved); err != nil {
			return broker.Session{}, err
		}
		request.Holder = opts.currentHolder(ctx)
		opts.diagnoseDeviceIdentity(b)
		var result broker.RegisterResult
		if err := call(ctx, upstream, "register_session", request, &result); err != nil {
			return broker.Session{}, err
		}
		opts.pruneMessageAck(b, d.registrationID, result.Code)
		if result.Code == broker.CodeRegistrationInUse {
			proof, err := opts.waitHolder(ctx, b, result.HolderChallenge)
			if err != nil {
				return broker.Session{}, err
			}
			request.HolderProof = proof
			continue
		}
		opts.clearHolderWait(b)
		if result.Code != "" && result.Code != broker.CodeStorage && result.Code != broker.CodeSuspended {
			opts.stopInitialBinding(b, identity)
		}
		if !repair && result.Code != "" && result.Code != broker.CodeStorage && result.Code != broker.CodeSuspended && result.Code != broker.CodeCapacity {
			return broker.Session{}, errAutomaticStopped
		}
		switch result.Code {
		case "":
			opts.updateCredentialScopes(b, upstream)
			if err := opts.negotiateMessageAck(b, result, identity.ChannelID, request.Capabilities); err != nil {
				return broker.Session{}, err
			}
			activityEnabled := negotiateActivity(ctx, upstream, result.ActivityVersion)
			b.mu.Lock()
			b.activityEnabled = activityEnabled
			b.runtimeInfoEnabled = result.RuntimeInfoVersion == broker.RuntimeInfoVersion
			b.ingressReady = result.Session.AutomaticDeliveryReady
			b.ingressReadyChannel, b.ingressReadyGeneration = identity.ChannelID, result.Session.Generation
			d.registrationID, d.name, d.revision = result.Session.RegistrationID, result.Session.Name, result.Session.Revision
			d.pendingRequest, d.pendingLease, d.pendingRevision = "", "", 0
			d.initialAttempt = initialComplete
			if !repair {
				b.temporary = true
			}
			d.handoverFrom = result.Session.HandoverFrom
			d.detached = false
			b.resumeStopReason = ""
			if b.started {
				// A rename on the live lease: the running receiver keeps serving
				// under the new name.
				b.identity.Name = result.Session.Name
			}
			saved := *d
			b.mu.Unlock()
			if err := opts.save(identity, &saved); err != nil {
				return broker.Session{}, err
			}
			if opts.bound != nil {
				opts.bound(result.Session)
			}
			return result.Session, nil
		case broker.CodeNotFound:
			if repaired[result.Code] || d.registrationID == "" {
				return broker.Session{}, errors.New("broker no longer knows this registration; register again")
			}
			repaired[result.Code] = true
			b.mu.Lock()
			d.registrationID, d.revision, d.pendingRequest = "", 0, ""
			b.mu.Unlock()
			request.ExpectedRevision = 0
		case broker.CodeRevisionMismatch:
			var current broker.Session
			if repaired[result.Code] || call(ctx, upstream, "get_session", map[string]string{"name": broker.SelectorRegistration + d.registrationID}, &current) != nil || current.Revision == 0 {
				return broker.Session{}, errors.New("registration changed elsewhere; stop the other local client and register again")
			}
			repaired[result.Code] = true
			b.mu.Lock()
			d.revision, d.pendingRequest = current.Revision, ""
			b.mu.Unlock()
			request.ExpectedRevision = current.Revision
		default:
			if result.Code == broker.CodeCapacity {
				return broker.Session{}, errors.New("remote session capacity reached; ask an administrator to detach old sessions before registering a new session")
			}
			return broker.Session{}, fmt.Errorf("broker rejected registration: %s", result.Code)
		}
	}
}

// receiverRetired fences a source as soon as a handover starts. The follow
// coordinator joins it after applying the handover; it must not recover the
// source credential in the meantime.
func receiverRetired(b *ingressBinding) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.retired || (b.durable != nil && b.durable.handedOver)
}

// runDurableReceiver keeps one receiver alive for the binding: after an
// upstream failure it resumes with a fresh lease; after a sink failure, an
// identity mismatch or a terminal broker answer it stops and reports.
func runDurableReceiver(ctx context.Context, upstream Upstream, b *ingressBinding, opts durableOptions, sink func(context.Context, broker.Message) error) {
	opts = opts.withDefaults()
	upstream = opts.wrapReceive(upstream, "")
	attempt := 0
	for {
		started := time.Now()
		opts.runProgress = &atomic.Bool{}
		exit := receiveDurableOnce(ctx, upstream, b, opts, sink)
		if opts.runProgress.Load() {
			attempt = 0
		}
		if exit == exitFollow {
			return
		}
		if receiverRetired(b) {
			// Moving to an unrelated saved conversation still retires a temporary
			// lease. Preserve its failure report so its transport can finish cleanly
			// before the coordinator sends the self-detach. A handed-over credential
			// is revoked and must never be used for that report.
			b.mu.Lock()
			id, registrationID, key, temporary := b.identity, b.durable.registrationID, b.durable.key, b.temporary
			b.mu.Unlock()
			if temporary && opts.state != nil {
				saved, found, err := opts.state.LoadSession(key)
				if err == nil && found && saved.Registration.HandedOverTo == "" && !upstreamRejected(upstream) {
					reportIngressFailure(upstream, id, registrationID)
				}
			}
			return
		}
		opts.diagnose(b, "receiver-exit", receiverReason(exit), "")
		if exit == exitSink {
			b.mu.Lock()
			native := b.nativeDeferred
			if native {
				b.receiveUnavailable = true
				b.receiveReason = "reconnect-required"
				b.receiveGate = ""
			}
			b.mu.Unlock()
			if native && ctx.Err() == nil && recoverDurable(ctx, upstream, b, opts) {
				continue
			}
		}
		if (exit == exitUpstream || exit == exitScopeDenied) && ctx.Err() == nil {
			// A successful resume is not evidence that the replacement can receive.
			if !opts.runProgress.Load() && time.Since(started) < opts.heartbeat {
				if err := opts.resumeClock.sleep(ctx, opts.backoff(attempt)); err != nil {
					return
				}
				attempt++
			}
			if receiverRetired(b) {
				return
			}
			if recoverDurable(ctx, upstream, b, opts) {
				continue
			}
		}
		b.mu.Lock()
		id, registrationID := b.identity, b.durable.registrationID
		b.mu.Unlock()
		reportIngressFailure(upstream, id, registrationID)
		return
	}
}

func receiveDurableOnce(ctx context.Context, upstream Upstream, b *ingressBinding, opts durableOptions, sink func(context.Context, broker.Message) error) receiverExit {
	runStarted := time.Now()
	if b.router != nil {
		b.router.nextGeneration()
		b.router.initSteerRecovery()
	}
	if opts.kimi != nil {
		opts.kimi.nextGeneration()
	}
	b.mu.Lock()
	ready := b.ingressReady
	b.mu.Unlock()
	if ready && opts.bindingIngressPending(b, upstream) {
		return exitScopeDenied
	}
	// The stream belongs to a lease; an already dequeued envelope belongs to
	// the binding. A failed renew must cancel the old stream, but not the native
	// post already in progress. Keep the binding cancellation for shutdown or
	// handover, and join the bounded sink before establishing a replacement.
	deliveryCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
	}()
	b.mu.Lock()
	id, registrationID := b.identity, b.durable.registrationID
	b.mu.Unlock()
	ackState := bindingMessageAck(b)
	var ackWake, ackFailed <-chan struct{}
	if ackState != nil {
		ackWake, ackFailed = ackState.wake, ackState.failed
	}
	permissionAllowed := !receiveDenied(upstream)
	activity := opts.newActivityStream(b)
	results := make(chan receiverExit, 2)
	workers.Add(2)
	go func() {
		defer workers.Done()
		timer := time.NewTimer(opts.heartbeat)
		defer timer.Stop()
		timeouts := 0
		for {
			heartbeat := false
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				heartbeat = time.Since(runStarted) >= opts.heartbeat
				activity.nextTick()
			case <-activity.wake:
				if !activity.transitionReady() {
					continue
				}
			case <-receiveWake(upstream):
			case <-ackWake:
			}
			if receiverRetired(b) {
				results <- exitFollow
				return
			}
			started := time.Now()
			renewCtx, stop := context.WithTimeout(ctx, opts.timeout)
			var result broker.RegisterResult
			sample := activity.sample(ctx, b, upstream)
			err := call(renewCtx, upstream, "renew_lease", renewMessageAck(broker.RenewRequest{RegistrationID: registrationID, ChannelID: id.ChannelID, IngressPending: opts.bindingIngressPending(b, upstream), Activity: sample, RuntimeInfo: activity.runtimeInfo}, ackState), &result)
			stop()
			interval := opts.heartbeat
			if errors.Is(err, context.DeadlineExceeded) {
				interval = min(interval, 5*time.Second)
			}
			// Retry from the attempt start, including time spent waiting for a reply.
			timer.Reset(max(0, interval-time.Since(started)))
			if receiverRetired(b) {
				results <- exitFollow
				return
			}
			if err == nil {
				opts.pruneMessageAck(b, registrationID, result.Code)
			}
			opts.diagnoseRenew(b, upstream, result, err)
			activity.diagnose(opts, b)

			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				// One slow renewal is tolerated; a hung connection is not.
				if timeouts++; timeouts < 3 {
					continue
				}
				results <- exitUpstream
				return
			}
			if err != nil {
				results <- exitUpstream
				return
			}
			timeouts = 0
			if ackState != nil && result.Code == "" && result.MessageAckVersion != 1 {
				results <- exitUpstream
				return
			}
			if result.ActivityVersion != broker.ActivityVersion {
				b.mu.Lock()
				b.activityEnabled = false
				b.mu.Unlock()
			}
			if result.RuntimeInfoVersion != broker.RuntimeInfoVersion {
				b.mu.Lock()
				b.runtimeInfoEnabled = false
				b.mu.Unlock()
			}
			switch result.Code {
			case "":
				b.mu.Lock()
				b.ingressReady = result.Session.AutomaticDeliveryReady
				b.ingressReadyChannel, b.ingressReadyGeneration = id.ChannelID, result.Session.Generation
				b.mu.Unlock()
				opts.updateCredentialScopes(b, upstream)
				allowed := !receiveDenied(upstream)
				if (result.Session.AutomaticDeliveryReady && opts.bindingIngressPending(b, upstream)) || allowed != permissionAllowed {
					results <- exitScopeDenied
					return
				}
				if result.PendingProbe != nil && answerProbe(ctx, upstream, b, opts, result.PendingProbe) {
					results <- exitTerminal
					return
				}
				if heartbeat {
					opts.touchOwnSession(b)
				}
				if heartbeat && opts.runProgress != nil {
					opts.runProgress.Store(true)
				}
				if b.router != nil {
					if err := b.router.flush(ctx); err != nil {
						results <- exitSink
						return
					}
				}
			case broker.CodeInvalid, broker.CodeSuspended:
				results <- exitUpstream
				return
			default:
				results <- exitTerminal
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		b.mu.Lock()
		unavailable := b.receiveUnavailable
		b.mu.Unlock()
		if receiveDenied(upstream) || unavailable {
			return
		}
		if opts.ready != nil {
			select {
			case <-opts.ready:
			case <-ctx.Done():
				return
			}
			// A newly issued legacy lease stays pending until this exact channel
			// is activated. Advertise readiness before opening its receive stream.
			activateCtx, stop := context.WithTimeout(ctx, opts.timeout)
			var result broker.RegisterResult
			err := call(activateCtx, upstream, "renew_lease", renewMessageAck(broker.RenewRequest{RegistrationID: registrationID, ChannelID: id.ChannelID}, ackState), &result)
			stop()
			if err == nil {
				opts.pruneMessageAck(b, registrationID, result.Code)
			}
			opts.diagnoseRenew(b, upstream, result, err)
			if err != nil {
				results <- exitUpstream
				return
			}
			if result.Code != "" {
				if result.Code == broker.CodeSuspended || result.Code == broker.CodeInvalid {
					results <- exitUpstream
					return
				}
				results <- exitTerminal
				return
			}
		}
		for {
			var message broker.Message
			// The stream belongs to this receiver, not a periodic poll. Cancelling
			// each idle wait sends a notification to the stateless HTTP server and
			// can close the shared MCP connection, including unrelated tool calls.
			// The registration, not its nickname, selects the stream, so a
			// rename on the live lease keeps it open.
			request := broker.IngressRequest{Recipient: broker.SelectorRegistration + registrationID, ChannelID: id.ChannelID}
			if ackState != nil {
				request.RegistrationID = registrationID
				request.Epoch = ackState.epoch
				request.AckThrough, request.Cursor = ackState.snapshot()
			}
			err := call(ctx, upstream, "wait_for_message", request, &message)
			if isCompatibilityError(err) {
				results <- exitTerminal
				return
			}
			if errors.Is(err, errInsufficientScope) {
				// Readiness is monotonic within a lease. Refresh it once as
				// pending, then run only renew/probe for this credential.
				results <- exitScopeDenied
				return
			}
			if err != nil {
				if ctx.Err() == nil && !receiverRetired(b) {
					class := diagnosticErrorClass(err, upstreamRejected(upstream))
					opts.diagnose(b, "receive-failed", class, class)
				}
				results <- exitUpstream
				return
			}
			// An envelope keeps the name it was accepted under, so the broker
			// names the registration whose queue delivered it. Older brokers,
			// which cannot rename, only address it to the current name.
			b.mu.Lock()
			current := b.identity.Name
			b.mu.Unlock()
			if message.RegistrationID != registrationID && (message.RegistrationID != "" || message.To != current) {
				results <- exitTerminal
				return
			}
			if ackState != nil {
				through, _ := ackState.snapshot()
				if message.DeliverySequence <= through {
					continue
				}
			}
			message, err = opts.receiveAttachment(ctx, upstream, b, message)
			if err != nil {
				results <- exitContext
				return
			}
			messageCtx := deliveryCtx
			var completion handoffCompletion
			if ackState != nil {
				skip, observeErr := ackState.observe(message.DeliverySequence)
				if observeErr != nil {
					results <- exitSink
					return
				}
				if skip {
					continue
				}
				completion = ackState.completion(deliveryCtx, b, message.DeliverySequence)
				done := completion.done
				completion.done = func() error {
					if err := done(); err != nil {
						return err
					}
					if ctx.Err() == nil && opts.runProgress != nil {
						opts.runProgress.Store(true)
					}
					return nil
				}
				messageCtx = context.WithValue(messageCtx, handoffKey{}, completion)
			}
			if b.router != nil && b.router.interceptSteerReplay(messageCtx, message) {
				continue
			}
			if err := sink(messageCtx, message); err != nil {
				if errors.Is(err, errFollowDelivery) {
					results <- exitFollow
					return
				}
				class := diagnosticErrorClass(err, false)
				opts.diagnose(b, "sink-failed", class, class)
				results <- exitSink
				return
			}
			if completion.done == nil && b.router == nil && opts.kimi == nil && opts.runProgress != nil {
				opts.runProgress.Store(true)
			}
			if completion.done != nil && b.router == nil && opts.kimi == nil {
				if err := completion.done(); err != nil {
					results <- exitSink
					return
				}
			}
		}
	}()
	select {
	case <-ackFailed:
		return exitSink
	case <-ctx.Done():
		return exitContext
	case exit := <-results:
		return exit
	}
}

// recoverDurable resumes the registration with a fresh lease until the broker
// answers. A transport failure retries the identical request so a resume the
// broker already applied is answered idempotently instead of fenced. Refresh
// after revision-mismatch is bounded; not-found, detached, owned and invalid
// answers are final because the automatic path never creates or steals.
func recoverDurable(ctx context.Context, upstream Upstream, b *ingressBinding, opts durableOptions) bool {
	if receiverRetired(b) {
		return false
	}
	opts.diagnose(b, "reconnect-start", "", "")
	_, ok := resumeDurable(ctx, upstream, b, opts, "")
	return ok
}

func resumeDurable(ctx context.Context, upstream Upstream, b *ingressBinding, opts durableOptions, lease string) (broker.Session, bool) {
	opts = opts.withDefaults()
	b.registerMu.Lock()
	defer b.registerMu.Unlock()
	mismatches := 0
	request := b.pendingResume
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil || receiverRetired(b) {
			return broker.Session{}, false
		}
		b.mu.Lock()
		id, d := b.identity, *b.durable
		b.mu.Unlock()
		if request.RequestID == "" {
			request = broker.ResumeRequest{
				Capabilities:   opts.deliveryCapabilities(ctx, upstream, id.Harness),
				RegistrationID: d.registrationID, Harness: id.Harness, SessionID: id.SessionID, PID: id.PID, CWD: id.CWD,
				ClientID: d.clientID, ChannelID: rand.Text(), Secret: d.secret, RequestID: rand.Text(), ExpectedRevision: d.revision,
				IngressPending: opts.bindingIngressPending(b, upstream),
			}
			if lease != "" {
				request.ChannelID = lease
			}
			b.pendingResume = request
		}
		request.Holder = opts.currentHolder(ctx)
		opts.diagnoseDeviceIdentity(b)
		callCtx, stop := context.WithTimeout(ctx, 2*opts.timeout)
		var result broker.RegisterResult
		err := call(callCtx, upstream, "resume_session", request, &result)
		stop()
		if receiverRetired(b) {
			return broker.Session{}, false
		}
		if err == nil {
			opts.pruneMessageAck(b, d.registrationID, result.Code)
		}
		if err == nil && result.Code == broker.CodeRegistrationInUse {
			proof, waitErr := opts.waitHolder(ctx, b, result.HolderChallenge)
			if waitErr != nil {
				return broker.Session{}, false
			}
			request.HolderProof = proof
			b.pendingResume = request
			continue
		}
		if err == nil {
			opts.clearHolderWait(b)
		}
		if err == nil && result.Code != broker.CodeSuspended {
			clearUpstreamSuspension(upstream)
		}
		b.mu.Lock()
		if err == nil {
			b.resumeSuspended = result.Code == broker.CodeSuspended
		} else if upstreamSuspended(upstream) {
			b.resumeSuspended = true
		}
		suspended := b.resumeSuspended
		b.mu.Unlock()
		if err != nil {
			class := diagnosticErrorClass(err, upstreamRejected(upstream))
			opts.diagnose(b, "reconnect-failed", class, class)
		} else if result.Code != "" {
			opts.diagnose(b, "reconnect-failed", diagnosticBrokerReason(result.Code), "protocol")
		}

		if terminalProjectError(err) {
			rememberProjectRefusal(b, upstream)
			b.mu.Lock()
			b.resumeStopReason = "project-deleted"
			b.receiveReason = err.Error()
			b.receiveGate = ""
			b.mu.Unlock()
			return broker.Session{}, false
		}
		if errors.Is(err, errInsufficientScope) || isCompatibilityError(err) {
			return broker.Session{}, false
		}
		if err != nil && upstreamRejected(upstream) {
			// The broker refused the session credential itself; retrying with
			// the same credential can never succeed.
			return broker.Session{}, false
		}
		if err == nil && result.Code == "" {
			opts.updateCredentialScopes(b, upstream)
			if err := opts.negotiateMessageAck(b, result, request.ChannelID, request.Capabilities); err != nil {
				return broker.Session{}, false
			}
			if b.router != nil && b.router.bindRegistration(result.Session.RegistrationID, result.Session.Epoch) {
				b.router.mu.Lock()
				diagnose := b.router.steer.diagnose
				b.router.mu.Unlock()
				if diagnose != nil {
					diagnose("steer-uncertain-retired")
				}
			}
			activityEnabled := negotiateActivity(ctx, upstream, result.ActivityVersion)
			b.pendingResume = broker.ResumeRequest{}
			b.mu.Lock()
			b.ingressReady = result.Session.AutomaticDeliveryReady
			b.ingressReadyChannel, b.ingressReadyGeneration = request.ChannelID, result.Session.Generation
			b.durable.detached = false
			b.durable.handoverFrom = result.Session.HandoverFrom
			b.resumeStopReason = ""
			b.activityEnabled = activityEnabled
			b.runtimeInfoEnabled = result.RuntimeInfoVersion == broker.RuntimeInfoVersion
			renamed := b.identity.Name != result.Session.Name
			b.identity.ChannelID = request.ChannelID
			b.identity.Name = result.Session.Name
			b.generation = result.Session.Generation
			b.durable.name, b.durable.revision = result.Session.Name, result.Session.Revision
			saved := *b.durable
			identity := b.identity
			b.mu.Unlock()
			_ = opts.save(identity, &saved)
			opts.diagnose(b, "reconnected", "", "")
			if renamed && opts.bound != nil {
				opts.bound(result.Session)
			}
			if result.PendingProbe != nil && answerProbe(ctx, upstream, b, opts, result.PendingProbe) {
				return broker.Session{}, false
			}
			return result.Session, true
		}
		if err == nil {
			// A definitive answer retires this request; the next attempt is new.
			b.pendingResume = broker.ResumeRequest{}
			request = broker.ResumeRequest{}
			switch result.Code {
			case broker.CodeRevisionMismatch:
				var current broker.Session
				reconcileErr := call(ctx, upstream, "get_session", map[string]string{"name": broker.SelectorRegistration + d.registrationID}, &current)
				if errors.Is(reconcileErr, errInsufficientScope) {
					return broker.Session{}, false
				}
				if reconcileErr != nil {
					// Suspension or a network interruption during reconciliation
					// is transient; it is not evidence of a conflicting revision.
					break
				}
				if mismatches++; mismatches > 3 || current.Revision == 0 {
					return broker.Session{}, false
				}
				b.mu.Lock()
				b.durable.revision = current.Revision
				b.mu.Unlock()
				continue
			case broker.CodeStorage, broker.CodeSuspended:
			default:
				if result.Code == broker.CodeDetached {
					opts.detachedObserved(b)
				}
				b.mu.Lock()
				switch result.Code {
				case broker.CodeNotFound, broker.CodeDetached, broker.CodeOwned, broker.CodeInvalid:
					b.resumeStopReason = "registration-" + result.Code
				default:
					b.resumeStopReason = "registration-invalid"
				}
				b.mu.Unlock()
				return broker.Session{}, false
			}
		}
		delay := 2 * time.Second
		if !suspended {
			delay = opts.backoff(attempt)
		}
		if err := opts.resumeClock.sleep(ctx, delay); err != nil {
			return broker.Session{}, false
		}
	}
}

// answerProbe re-observes the bound parent session locally and reports
// whether it is still the registered one. An observation failure leaves the
// probe pending for the next renewal instead of reporting a mismatch. It
// returns true when the receiver must stop after removal or an identity mismatch.
func answerProbe(ctx context.Context, upstream Upstream, b *ingressBinding, opts durableOptions, probe *broker.PendingProbe) bool {
	b.mu.Lock()
	id, registrationID := b.identity, b.durable.registrationID
	b.mu.Unlock()
	observeCtx, stop := context.WithTimeout(ctx, 3*opts.timeout)
	observed, err := opts.observe(observeCtx, id)
	stop()
	if err != nil {
		return false
	}
	ok := observed.Harness == id.Harness && observed.SessionID == id.SessionID && observed.PID == id.PID && sameCWD(observed.CWD, id.CWD)
	answer := broker.ProbeAnswer{RegistrationID: registrationID, ProbeID: probe.ID, Revision: probe.Revision, ChannelID: id.ChannelID, OK: ok, PID: observed.PID, CWD: observed.CWD}
	answerCtx, stopAnswer := context.WithTimeout(ctx, opts.timeout)
	defer stopAnswer()
	var result broker.ProbeResult
	if err := call(answerCtx, upstream, "probe_response", answer, &result); err != nil {
		class := diagnosticErrorClass(err, upstreamRejected(upstream))
		opts.diagnose(b, "probe-failed", class, class)
		return false
	}
	opts.pruneMessageAck(b, registrationID, result.Code)
	if !ok {
		opts.diagnose(b, "probe-failed", "identity-mismatch", "other")
	}

	return result.Removed || (!ok && result.Code == "")
}

// An unavailable inbox keeps a valid registration alive without opening a
// competing receiver or advertising a transport it cannot deliver through.
func (o durableOptions) bindingIngressPending(b *ingressBinding, upstream Upstream) bool {
	b.mu.Lock()
	unavailable := b.receiveUnavailable
	b.mu.Unlock()
	return unavailable || o.ingressPending() || receiveDenied(upstream)
}

// A live binding stays fresh after exit without refreshing discovery candidates.
func (o durableOptions) touchOwnSession(b *ingressBinding) {
	if o.state == nil {
		return
	}
	now := o.now()
	b.mu.Lock()
	d := b.durable
	if d.session == nil || d.key == "" || now.Before(d.lastSessionTouch.Add(24*time.Hour)) {
		b.mu.Unlock()
		return
	}
	d.lastSessionTouch = now
	key := d.key
	b.mu.Unlock()
	_, _, _ = o.state.LoadSession(key)
}
