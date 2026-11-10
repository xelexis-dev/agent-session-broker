package channel

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type followRoster struct {
	mu sync.Mutex
	id broker.AttachRequest
}

func (r *followRoster) observe(context.Context, broker.AttachRequest) (broker.AttachRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.id, nil
}
func (r *followRoster) set(id broker.AttachRequest) { r.mu.Lock(); r.id = id; r.mu.Unlock() }

func TestResumeFollowTemporaryToStoredConversation(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	bID := claudeHandoverIdentity(201, 4201)
	b := r.identityHarness(t, dir, bID, handoverOptions)
	b.attach(t)
	original := b.session
	b.close()
	pID := claudeHandoverIdentity(202, 4201)
	store, err := clientstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cid, err := store.ClientID()
	if err != nil {
		t.Fatal(err)
	}
	scope := brokerScope(r.endpoint, "detach")
	// A compatible unfinished initial attempt is automatically completed by this connection.
	pending := clientstate.SessionState{Scope: scope, Project: "detach", Harness: "claude", SessionID: pID.SessionID, ClientID: cid, Registration: clientstate.Registration{Secret: clientstate.NewSecret(), InitialAttempt: initialPending}}
	if err = store.SaveSession(clientstate.SessionKey(scope, "claude", pID.SessionID, cid), pending); err != nil {
		t.Fatal(err)
	}
	roster := &followRoster{id: pID}
	nativeReceived := followNativeInbox(t, pID)
	h := r.identityHarness(t, dir, pID, func(o *durableOptions) {
		handoverOptions(o)
		o.observe = roster.observe
		o.claudeStartupClock = instantClaudeClock()
		o.claudeInboxFactory = newClaudeInboxFactory(t.Context(), pID, roster.observe, o.parentProcessKey)
	})
	waitFor(t, "placeholder registered automatically", func() bool { return h.status(t).Registered })
	placeholder := h.status(t).RegistrationID
	if placeholder == original.RegistrationID {
		t.Fatal("setup did not create temporary registration")
	}
	roster.set(bID)
	waitFor(t, "stored conversation followed without reconnect", func() bool {
		s := h.status(t)
		return s.Registered && s.RegistrationID == original.RegistrationID && s.Name == original.Name
	})
	manager := r.harness(t, t.TempDir(), 4202)
	manager.attach(t)
	if err := call(t.Context(), manager.client, "send_message", broker.Message{From: manager.session.Name, To: original.Name, Body: "followed-message"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-nativeReceived:
		if m.Body != "followed-message" {
			t.Fatal("wrong message")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("native inbox did not receive followed message")
	}
	var old broker.Session
	if err := call(t.Context(), manager.client, "get_session", map[string]string{"name": placeholder}, &old); err != nil || old.ErrorCode != broker.CodeDetached {
		t.Fatal("temporary registration was not detached")
	}
}

func followOptions(roster *followRoster) func(*durableOptions) {
	return func(o *durableOptions) {
		handoverOptions(o)
		o.observe = roster.observe
		o.claudeStartupClock = instantClaudeClock()
	}
}

func TestResumeFollowHandoverKeepsUsedRegistrationAndQueue(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	aID := claudeHandoverIdentity(210, 4301)
	roster := &followRoster{id: aID}
	var connections atomic.Int32
	h := r.identityHarness(t, dir, aID, func(o *durableOptions) {
		followOptions(roster)(o)
		original := o.device.newUpstream
		o.device.newUpstream = func(token string) Upstream {
			u := original(token)
			if connections.Add(1) == 1 {
				return &handoverBlockedReceiver{u.(*brokerUpstream)}
			}
			return u
		}
	})
	h.attach(t)
	original := h.session
	if err := call(t.Context(), h.client, "rename_session", map[string]string{"name": "used-follow-session"}, &h.session); err != nil {
		t.Fatal(err)
	}
	manager := r.harness(t, t.TempDir(), 4302)
	manager.attach(t)
	if err := call(t.Context(), manager.client, "send_message", broker.Message{From: manager.session.Name, To: h.session.Name, Body: "queued-follow"}, nil); err != nil {
		t.Fatal(err)
	}
	next := aID
	next.SessionID = claudeHandoverIdentity(211, 4301).SessionID
	roster.set(next)
	waitFor(t, "automatic handover", func() bool {
		s := h.status(t)
		return s.Registered && s.RegistrationID == original.RegistrationID && strings.Contains(s.Message, "followed: handover")
	})
	select {
	case m := <-h.received:
		if m.Body != "queued-follow" {
			t.Fatal("wrong queue message")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handover lost queued message")
	}
	var session broker.Session
	if err := call(t.Context(), manager.client, "get_session", map[string]string{"name": original.RegistrationID}, &session); err != nil || session.Name != "used-follow-session" || session.ErrorCode == broker.CodeDetached {
		t.Fatal("used registration/name was not preserved")
	}
}

func TestResumeFollowStoredTargetGuards(t *testing.T) {
	for _, kind := range []string{"suspended", "detached", "still-receiving"} {
		t.Run(kind, func(t *testing.T) {
			r := newDetachedRuntime(t)
			dir := t.TempDir()
			targetID := claudeHandoverIdentity(220, 4401)
			target := r.identityHarness(t, dir, targetID, handoverOptions)
			target.attach(t)
			manager := r.harness(t, t.TempDir(), 4402)
			manager.attach(t)
			if kind != "still-receiving" {
				name := "suspend_session"
				if kind == "detached" {
					name = "stop_session"
				}
				if err := call(t.Context(), manager.client, name, map[string]string{"name": target.session.RegistrationID}, nil); err != nil {
					t.Fatal(err)
				}
				target.close()
			}
			aID := claudeHandoverIdentity(221, 4403)
			roster := &followRoster{id: aID}
			h := r.identityHarness(t, dir, aID, followOptions(roster))
			h.attach(t)
			original := h.session
			before := r.direct.Load()
			switched := targetID
			switched.PID = aID.PID
			roster.set(switched)
			waitFor(t, "safe target refusal", func() bool { return strings.Contains(h.status(t).Message, "follow-skipped: "+kind) })
			if r.direct.Load() != before {
				t.Fatal("refused target started admission")
			}
			if s := h.status(t); s.RegistrationID != original.RegistrationID {
				t.Fatal("refused target replaced source")
			}
		})
	}
}

func TestResumeFollowParentAndObservationGuards(t *testing.T) {
	for _, kind := range []string{"pid", "cwd", "key", "roster-error", "unstable", "unsupported"} {
		t.Run(kind, func(t *testing.T) {
			a := claudeHandoverIdentity(230, 4501)
			next := claudeHandoverIdentity(231, 4501)
			opts := durableOptions{device: &deviceAuthOptions{}, claudeStartupClock: instantClaudeClock(), parentProcessKey: func(context.Context, int) (string, error) { return "initial", nil }}
			var calls int
			opts.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) {
				calls++
				if kind == "roster-error" {
					return broker.AttachRequest{}, errors.New("synthetic roster unavailable")
				}
				if kind == "unstable" && calls%2 == 0 {
					return a, nil
				}
				return next, nil
			}
			f := newClaudeFollow(a, &opts)
			auth := &sessionAuth{state: clientstate.SessionState{LastAdmissionMode: "auto"}}
			binding := &ingressBinding{mu: &sync.Mutex{}, started: true, durable: &durableRegistration{session: auth}}
			f.lookup = func(string) *ingressBinding { return binding }
			moves := 0
			f.move = func(context.Context, broker.AttachRequest, broker.AttachRequest) (broker.AttachRequest, error) {
				moves++
				return next, nil
			}
			switch kind {
			case "pid":
				next.PID++
			case "cwd":
				next.CWD = "/other"
			case "key":
				opts.parentProcessKey = func(context.Context, int) (string, error) { return "changed", nil }
			case "unsupported":
				f.key = ""
			}
			got, err := f.check(t.Context(), a)
			if err != nil || got.SessionID != a.SessionID || moves != 0 {
				t.Fatal("unsafe observation followed")
			}
		})
	}
}

func TestResumeFollowDeliverySwitchIsOnce(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	a := claudeHandoverIdentity(240, 4601)
	roster := &followRoster{id: a}
	h := r.identityHarness(t, dir, a, followOptions(roster))
	h.attach(t)
	original := h.session
	manager := r.harness(t, t.TempDir(), 4602)
	manager.attach(t)
	next := claudeHandoverIdentity(241, 4601)
	roster.set(next)
	if err := call(t.Context(), manager.client, "send_message", broker.Message{From: manager.session.Name, To: original.Name, Body: "delivery-switch"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-h.received:
		if m.Body != "delivery-switch" {
			t.Fatal("wrong message")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("delivery did not follow before reconnect-required")
	}
	select {
	case <-h.received:
		t.Fatal("duplicate delivery")
	case <-time.After(100 * time.Millisecond):
	}
	if s := h.status(t); !s.Registered || s.RegistrationID != original.RegistrationID || !strings.Contains(s.Message, "followed: handover") {
		t.Fatal("delivery failed to switch binding")
	}
}

type followTick struct {
	delay   time.Duration
	release chan struct{}
}
type followClock struct {
	mu    sync.Mutex
	now   time.Time
	ticks chan followTick
}

func (f *followClock) clock() devicePollClock {
	return devicePollClock{now: func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }, sleep: func(ctx context.Context, d time.Duration) error {
		tick := followTick{d, make(chan struct{})}
		select {
		case f.ticks <- tick:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-tick.release:
			f.mu.Lock()
			f.now = f.now.Add(d)
			f.mu.Unlock()
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
}
func TestResumeFollowPeriodicWithoutToolsOrReception(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	a := claudeHandoverIdentity(250, 4701)
	roster := &followRoster{id: a}
	clock := &followClock{now: time.Unix(1000, 0), ticks: make(chan followTick, 1)}
	h := r.identityHarness(t, dir, a, func(o *durableOptions) { followOptions(roster)(o); o.claudeFollowClock = clock.clock() })
	h.attach(t)
	next := claudeHandoverIdentity(251, 4701)
	roster.set(next)
	select {
	case tick := <-clock.ticks:
		if tick.delay != 2*time.Second {
			t.Fatal("startup interval differs")
		}
		close(tick.release)
	case <-time.After(time.Second):
		t.Fatal("monitor not scheduled")
	}
	// Read server state through a different client, never invoke the followed client.
	manager := r.harness(t, t.TempDir(), 4702)
	manager.attach(t)
	waitFor(t, "periodic handover without local calls", func() bool {
		var s broker.Session
		return call(t.Context(), manager.client, "get_session", map[string]string{"name": h.session.RegistrationID}, &s) == nil && s.ID == next.SessionID && s.AutomaticDeliveryReady
	})
}
func TestResumeFollowPollIntervals(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	id := claudeHandoverIdentity(260, 4801)
	f := &claudeFollow{began: time.Unix(1000, 0)}
	if got := f.interval(f.began.Add(4*time.Minute), id); got != 2*time.Second {
		t.Fatal("missing record must poll quickly")
	}
	project := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, id.CWD)
	path := filepath.Join(root, "projects", project, id.SessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if got := f.interval(f.began.Add(time.Minute), id); got != 2*time.Second {
		t.Fatal("startup must poll quickly")
	}
	if got := f.interval(f.began.Add(3*time.Minute), id); got != 30*time.Second {
		t.Fatal("settled record must poll every 30 seconds")
	}
}

func followNativeInbox(t *testing.T, id broker.AttachRequest) <-chan broker.DeliveredMessage {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix native inbox fixture")
	}
	dir := shortSocketDir(t, "follow-inbox-")
	path := filepath.Join(dir, "i.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	t.Setenv("CLAUDE_CODE_SESSION_ID", id.SessionID)
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "synthetic-only")
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", path)
	received := make(chan broker.DeliveredMessage, 10)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			decoder := json.NewDecoder(conn)
			var auth struct{ Type, Token string }
			var frame struct {
				Type    string
				Message struct{ Role, Content string }
			}
			valid := decoder.Decode(&auth) == nil && auth.Type == "auth" && auth.Token == "synthetic-only" && decoder.Decode(&frame) == nil && frame.Type == "user"
			conn.Close()
			if valid {
				var m broker.DeliveredMessage
				if json.Unmarshal([]byte(frame.Message.Content), &m) == nil {
					received <- m
				}
			}
		}
	}()
	return received
}

type followLegacyAuthorizer struct{ *deviceAuthClient }

func (a *followLegacyAuthorizer) exchange(ctx context.Context, r directCodeRequest) (directGrant, error) {
	if r.Handover {
		return directGrant{}, broker.HandoverRefusal{Reason: "unsupported-mode"}
	}
	return a.deviceAuthClient.exchange(ctx, r)
}
func TestResumeFollowLegacyRefusalDoesNotRegister(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	a := claudeHandoverIdentity(270, 4901)
	roster := &followRoster{id: a}
	h := r.identityHarness(t, dir, a, func(o *durableOptions) {
		followOptions(roster)(o)
		o.device.authorizer = &followLegacyAuthorizer{o.device.authorizer.(*deviceAuthClient)}
	})
	h.attach(t)
	original := h.session
	next := claudeHandoverIdentity(271, 4901)
	roster.set(next)
	before := r.direct.Load()
	waitFor(t, "legacy refusal guidance", func() bool { return strings.Contains(h.status(t).Message, "follow-skipped: unsupported-mode") })
	if s := h.status(t); s.RegistrationID != original.RegistrationID || r.direct.Load() != before {
		t.Fatal("legacy refusal started admission or replaced source")
	}
}
func TestResumeFollowConcurrentTransitionsKeepOneRegistration(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	a := claudeHandoverIdentity(280, 5001)
	roster := &followRoster{id: a}
	h := r.identityHarness(t, dir, a, followOptions(roster))
	h.attach(t)
	original := h.session
	for _, n := range []int{281, 282} {
		next := claudeHandoverIdentity(n, 5001)
		roster.set(next)
		var wg sync.WaitGroup
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var s authStatus
				_ = call(t.Context(), h.client, "auth_status", map[string]any{}, &s)
			}()
		}
		wg.Wait()
		s := h.status(t)
		if !s.Registered || s.RegistrationID != original.RegistrationID || s.Name != original.Name {
			t.Fatal("overlapping transitions replaced registration")
		}
	}
	var sessions []broker.Session
	if err := call(t.Context(), h.client, "list_sessions", map[string]any{}, &sessions); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, s := range sessions {
		if s.Harness == "claude" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("overlapping transitions created duplicate registrations")
	}
}

type followInterruptedResume struct {
	*brokerUpstream
	block   *atomic.Bool
	started chan struct{}
}

func (u *followInterruptedResume) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name == "resume_session" && u.block.Swap(false) {
		close(u.started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return u.brokerUpstream.CallTool(ctx, p)
}
func TestResumeFollowCanceledAfterHandoverResumesWithNewCredential(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	a := claudeHandoverIdentity(290, 5101)
	roster := &followRoster{id: a}
	var block atomic.Bool
	started := make(chan struct{})
	h := r.identityHarness(t, dir, a, func(o *durableOptions) {
		followOptions(roster)(o)
		original := o.device.newUpstream
		o.device.newUpstream = func(token string) Upstream {
			return &followInterruptedResume{original(token).(*brokerUpstream), &block, started}
		}
	})
	h.attach(t)
	original := h.session
	block.Store(true)
	next := claudeHandoverIdentity(291, 5101)
	roster.set(next)
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, _ = h.client.CallTool(ctx, &mcp.CallToolParams{Name: "auth_status", Arguments: map[string]any{}})
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("handover did not reach resume")
	}
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled tool did not finish")
	}
	waitFor(t, "saved handover resumed after caller cancellation", func() bool {
		s := h.status(t)
		return s.Registered && s.RegistrationID == original.RegistrationID && strings.Contains(s.Message, "followed: resumed")
	})
	if r.direct.Load() != 2 {
		t.Fatal("retry repeated admission instead of persisted resume")
	}
}

func TestResumeFollowTransientDeliveryObservationKeepsReceiver(t *testing.T) {
	r := newDetachedRuntime(t)
	a := claudeHandoverIdentity(301, 5301)
	other := claudeHandoverIdentity(302, 5301)
	var armed atomic.Bool
	h := r.identityHarness(t, t.TempDir(), a, func(o *durableOptions) {
		handoverOptions(o)
		o.claudeStartupClock = instantClaudeClock()
		o.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) {
			if armed.Swap(false) {
				return other, nil
			}
			return a, nil
		}
	})
	h.attach(t)
	manager := r.harness(t, t.TempDir(), 5302)
	manager.attach(t)
	armed.Store(true)
	for _, body := range []string{"transient-first", "transient-second"} {
		if err := call(t.Context(), manager.client, "send_message", broker.Message{From: manager.session.Name, To: h.session.Name, Body: body}, nil); err != nil {
			t.Fatal(err)
		}
		select {
		case m := <-h.received:
			if m.Body != body {
				t.Fatal("delivery duplicated or reordered")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("transient roster change stopped delivery")
		}
	}
	select {
	case <-h.received:
		t.Fatal("duplicate message")
	case <-time.After(100 * time.Millisecond):
	}
	status := h.status(t)
	if !status.Registered || status.ReceiveError != "" || strings.Contains(status.Message, "reconnect-required") {
		t.Fatal("transient observation left receiver unavailable")
	}
}

func TestResumeFollowRedeliverySinkFailureIsDiagnosed(t *testing.T) {
	for _, kind := range []string{"nil", "error"} {
		t.Run(kind, func(t *testing.T) {
			store, err := clientstate.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			initial := claudeHandoverIdentity(310, 5401)
			next := claudeHandoverIdentity(311, 5401)
			opts := durableOptions{state: store, device: &deviceAuthOptions{}, claudeStartupClock: instantClaudeClock(), parentProcessKey: func(context.Context, int) (string, error) { return "same", nil }, observe: func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return next, nil }}
			auth := &sessionAuth{state: clientstate.SessionState{LastAdmissionMode: "auto"}}
			source := &ingressBinding{mu: &sync.Mutex{}, started: true, identity: initial, durable: &durableRegistration{key: strings.Repeat("a", 32), session: auth}, followMessage: &broker.Message{Body: "redeliver"}}
			target := &ingressBinding{mu: &sync.Mutex{}, started: true, identity: next, durable: &durableRegistration{key: strings.Repeat("b", 32)}}
			if kind == "error" {
				target.sink = func(context.Context, broker.Message) error { return errors.New("synthetic sink failure") }
			}
			f := newClaudeFollow(initial, &opts)
			f.lookup = func(id string) *ingressBinding {
				if id == initial.SessionID {
					return source
				}
				return target
			}
			f.move = func(context.Context, broker.AttachRequest, broker.AttachRequest) (broker.AttachRequest, error) {
				return next, nil
			}
			_, err = f.check(t.Context(), initial)
			if err != nil {
				t.Fatal(err)
			}
			if target.receiveReason != "reconnect-required" || !target.receiveUnavailable {
				t.Fatal("failed redelivery silently discarded")
			}
			diagnostics := opts.diagnostics(target)
			if diagnostics == nil || diagnostics.LastFailureReason != "sink-failed" || diagnostics.LastFailureCode != "other" {
				t.Fatal("sink failure diagnostic missing")
			}
		})
	}
}

func TestResumeFollowFallbackRejectingSinkHasReason(t *testing.T) {
	store, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := claudeHandoverIdentity(320, 5501)
	opts := durableOptions{state: store, device: &deviceAuthOptions{}, parentProcessKey: func(context.Context, int) (string, error) { return "same", nil }, observe: func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return a, nil }}
	source := &ingressBinding{mu: &sync.Mutex{}, identity: a, started: true, durable: &durableRegistration{key: strings.Repeat("c", 32), session: &sessionAuth{state: clientstate.SessionState{LastAdmissionMode: "auto"}}}, followMessage: &broker.Message{Body: "pending"}, sink: func(context.Context, broker.Message) error { return errors.New("synthetic inbox rejection") }}
	f := newClaudeFollow(a, &opts)
	f.lookup = func(string) *ingressBinding { return source }
	f.move = func(context.Context, broker.AttachRequest, broker.AttachRequest) (broker.AttachRequest, error) {
		t.Fatal("same identity moved")
		return a, nil
	}
	restored := false
	f.restore = func(context.Context, *ingressBinding) error { restored = true; return nil }
	f.redeliver(t.Context(), a, broker.Message{})
	if restored || source.receiveReason != "reconnect-required" || !source.receiveUnavailable {
		t.Fatal("rejected source sink left silent failure")
	}
	summary := opts.diagnostics(source)
	if summary.LastFailureReason != "sink-failed" {
		t.Fatal("source sink failure diagnostic absent")
	}
}

func TestResumeFollowDetachedSourceToNewConversationFailsClosed(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	source := claudeHandoverIdentity(290, 4901)
	next := claudeHandoverIdentity(291, source.PID)
	roster := &followRoster{id: source}
	clock := &followClock{now: time.Unix(1000, 0), ticks: make(chan followTick, 1)}
	observed := make(chan struct{})
	proceed := make(chan struct{})
	var observedOnce, proceedOnce sync.Once
	defer proceedOnce.Do(func() { close(proceed) })
	var options *durableOptions
	h := r.identityHarness(t, dir, source, func(o *durableOptions) {
		options = o
		followOptions(roster)(o)
		o.claudeFollowClock = clock.clock()
		o.observe = func(ctx context.Context, initial broker.AttachRequest) (broker.AttachRequest, error) {
			id, err := roster.observe(ctx, initial)
			if id.SessionID == next.SessionID {
				observedOnce.Do(func() { close(observed) })
				select {
				case <-proceed:
				case <-ctx.Done():
					return id, ctx.Err()
				}
			}
			return id, err
		}
	})
	h.attach(t)
	original := h.session
	store, key, _ := handoverSaved(t, r, dir, source)
	manager := r.harness(t, t.TempDir(), 4902)
	manager.attach(t)
	binding := options.claudeFollow.lookup(source.SessionID)
	binding.mu.Lock()
	receiverDone := binding.done
	binding.mu.Unlock()
	roster.set(next)
	select {
	case tick := <-clock.ticks:
		close(tick.release)
	case <-time.After(5 * time.Second):
		t.Fatal("follow monitor tick unavailable")
	}
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("automatic follow did not select source before detach")
	}
	if err := call(t.Context(), manager.client, "stop_session", map[string]string{"name": original.RegistrationID}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "source receiver persisted detach during follow", func() bool {
		saved, found, err := store.LoadSession(key)
		return err == nil && found && saved.Registration.Detached
	})
	select {
	case <-receiverDone:
	case <-time.After(5 * time.Second):
		t.Fatal("detached source receiver did not exit")
	}
	newConversationRequests := func() int {
		count := 0
		for _, request := range r.requestSnapshot() {
			if request.session == next.SessionID {
				count++
			}
		}
		return count
	}
	before := newConversationRequests()
	if before != 0 {
		t.Fatal("new conversation admitted before follow continued")
	}
	proceedOnce.Do(func() { close(proceed) })
	waitFor(t, "automatic follow completed", func() bool { return len(clock.ticks) > 0 })
	if options.claudeFollow.summary() != "follow-skipped: detached" {
		t.Fatal("automatic follow did not refuse detached source")
	}
	if newConversationRequests() != before {
		t.Fatal("automatic follow admitted or resumed new conversation")
	}

	var remote broker.Session
	if err := call(t.Context(), manager.client, "get_session", map[string]string{"name": original.RegistrationID}, &remote); err != nil || remote.ErrorCode != broker.CodeDetached {
		t.Fatal("automatic follow revived server registration")
	}
}
