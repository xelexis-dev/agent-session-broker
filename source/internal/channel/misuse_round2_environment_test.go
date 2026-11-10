package channel

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type round2CallFunc func(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error)

func (f round2CallFunc) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return f(ctx, p)
}

func TestRound2ScopeChangeNeverExportsSavedProofOrJournals(t *testing.T) {
	for _, mode := range []string{"server-url", "project"} {
		for _, manual := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "/direct", true: "/manual"}[manual], func(t *testing.T) {
				store, err := clientstate.Open(t.TempDir())
				if err != nil {
					t.Fatal("state open failed")
				}
				cid, err := store.ClientID()
				if err != nil {
					t.Fatal("client id failed")
				}
				id := claudeHandoverIdentity(1701, 21701)
				oldScope := brokerScope("https://old.invalid/mcp?project=brk", "brk")
				nextScope := brokerScope("https://new.invalid/mcp?project=brk", "brk")
				nextProject := "brk"
				if mode == "project" {
					nextProject = "other"
					nextScope = brokerScope("https://old.invalid/mcp?project=other", "other")
				}
				oldKey := clientstate.SessionKey(oldScope, "claude", id.SessionID, cid)
				oldRID := "AAAAAAAAAAAAAAAAAAAAAAAAAA"
				oldSecret := clientstate.NewSecret()
				oldCredential := "synthetic-old-broker-credential"
				oldRegistration := clientstate.Registration{RegistrationID: oldRID, Name: "old-broker-only", Secret: oldSecret, Revision: 2, ParentPID: id.PID, ProcessKey: "same-parent", Harness: "claude", SessionID: id.SessionID, InitialAttempt: initialComplete}
				saved := clientstate.SessionState{Scope: oldScope, Project: "brk", Harness: "claude", SessionID: id.SessionID, ClientID: cid, Registration: oldRegistration, Credential: &clientstate.SessionCredential{AccessToken: oldCredential, AdmissionMode: "auto"}, LastAdmissionMode: "auto", Direct: &clientstate.DirectAuthorization{RequestID: "old-direct-request", Status: "requested", CreatedAt: time.Now()}, Handover: &clientstate.HandoverAttempt{Manual: manual, SourceKey: oldKey, RequestID: "old-handover-request", Registration: oldRegistration}}
				if manual {
					saved.LastAdmissionMode = "manual"
					saved.Credential.AdmissionMode = "manual"
					saved.Device = &clientstate.DeviceAuthorization{Status: clientstate.DevicePending, RequestID: "old-device-request", DeviceCode: "old-device-only"}
				}
				if err := store.SaveSession(oldKey, saved); err != nil {
					t.Fatal("old session setup failed")
				}
				if err := store.SaveRegistration(clientstate.RegistrationKey(oldScope, "claude", id.SessionID), oldRegistration); err != nil {
					t.Fatal("legacy setup failed")
				}
				oldAt := time.Now().UTC().Add(-time.Hour)
				if err := store.RecordDiagnostic(oldKey, clientstate.DiagnosticEvent{At: oldAt, SessionID: id.SessionID, RegistrationID: oldRID, Event: "reconnect-failed", Reason: "network", ErrorClass: "network"}); err != nil {
					t.Fatal("diagnostic setup failed")
				}
				auth := &scriptedDirect{}
				opts := (durableOptions{state: store, scope: nextScope, project: nextProject, device: &deviceAuthOptions{authorizer: auth, clock: realPollClock()}, parentProcessKey: func(context.Context, int) (string, error) { return "same-parent", nil }}).withDefaults()
				d, release, err := opts.loadSessionRegistration(id)
				if err != nil {
					t.Fatal("new scope failed to load")
				}
				defer release()
				b := &ingressBinding{mu: &sync.Mutex{}, identity: id, durable: d}
				if opts.automaticRegistrationAllowed(b) || d.registrationID != "" || d.secret == oldSecret || d.session.state.Handover != nil || d.session.state.Direct != nil || d.session.state.Credential != nil {
					t.Fatal("new scope inherited authorization or ownership")
				}
				explicit := context.WithValue(t.Context(), manualExplicitAttachKey{}, true)
				if err := opts.prepareConversationHandover(context.WithValue(explicit, takeoverContextKey{}, oldRegistration.Name), b, id); err == nil {
					t.Fatal("foreign scope name was a takeover candidate")
				}
				if len(auth.requests) != 0 {
					t.Fatal("foreign takeover triggered exchange")
				}
				if err := opts.prepareConversationHandover(explicit, b, id); err != nil {
					t.Fatal("fresh explicit preparation failed")
				}
				if err := opts.beginInitialRegistration(b); err != nil {
					t.Fatal("fresh permission save failed")
				}
				handled, err := opts.tryDirect(t.Context(), d.session, id)
				if !handled || err != nil || len(auth.requests) != 1 {
					t.Fatal("fresh explicit exchange failed")
				}
				if auth.requests[0].RegistrationID != "" || auth.requests[0].Secret == oldSecret || auth.requests[0].Handover {
					t.Fatal("old proof escaped in direct request")
				}
				if err := opts.finishManualHandover(t.Context(), b, id); err != nil {
					t.Fatal("old manual journal was reused")
				}
				calls := 0
				target := round2CallFunc(func(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
					calls++
					raw, _ := json.Marshal(p.Arguments)
					for _, value := range []string{oldSecret, oldCredential, oldRID, "old-handover-request", "old-direct-request", "old-device-only"} {
						if strings.Contains(string(raw), value) {
							t.Fatal("old proof escaped in upstream request")
						}
					}
					if p.Name != "register_session" {
						t.Fatal("new scope attempted resume or rebind")
					}
					return &mcp.CallToolResult{StructuredContent: broker.RegisterResult{Session: broker.Session{RegistrationID: "BBBBBBBBBBBBBBBBBBBBBBBBBB", Name: "new-broker-worker", Revision: 1}}}, nil
				})
				if _, err := registerDurable(t.Context(), target, b, opts, id); err != nil || calls != 1 {
					t.Fatal("new scope did not create exactly one fresh registration after explicit attach")
				}
				diagnostics := opts.bugDiagnostics(t.Context(), b, target)
				for _, event := range diagnostics.Events {
					if event.At.Equal(oldAt) {
						t.Fatal("old scope diagnostics entered bug attachment")
					}
				}
				raw, _ := json.Marshal(diagnostics)
				for _, value := range []string{oldSecret, oldCredential, oldRID, oldKey} {
					if strings.Contains(string(raw), value) {
						t.Fatal("old scope proof entered diagnostic attachment")
					}
				}
			})
		}
	}
}

func TestRound2NestedClaudeInheritedMarkersCannotBindParent(t *testing.T) {
	outer := claudeHandoverIdentity(1710, 21710)
	inner := claudeHandoverIdentity(1711, 21711)
	t.Setenv("CLAUDE_CODE_SESSION_ID", outer.SessionID)
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "synthetic-inherited-only")
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "/private/tmp/synthetic-parent-only.sock")
	polls := 0
	got, err := nearestClaudeIdentity(t.Context(), inner.PID, func(context.Context) ([]rosterRow, error) {
		polls++
		rows := []rosterRow{{SessionID: outer.SessionID, PID: outer.PID, Kind: "interactive"}}
		if polls > 1 {
			rows = append(rows, rosterRow{SessionID: inner.SessionID, PID: inner.PID, Kind: "interactive"})
		}
		return rows, nil
	}, func(_ context.Context, sid string) (broker.AttachRequest, error) {
		if sid != inner.SessionID {
			t.Fatal("outer session was selected")
		}
		return inner, nil
	}, time.Second, time.Millisecond)
	if err != nil || !reflect.DeepEqual(got, inner) {
		t.Fatal("nested child did not select its exact ancestor")
	}
	factory := newClaudeInboxFactory(t.Context(), got, func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return inner, nil }, func(context.Context, int) (string, error) { return "child-process", nil })
	sink, _, inboxErr := factory(inner)
	if inboxErr == nil {
		inboxErr = sink(t.Context(), broker.Message{From: "peer", To: "child", Body: "must-not-deliver"})
	}
	if inboxErr == nil {
		t.Fatal("child reused the inherited parent inbox")
	}
	// Candidate selection must use the child's PID/process key, not env ID.
	store, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal("state open failed")
	}
	cid, _ := store.ClientID()
	scope := "nested-scope"
	oldKey := clientstate.SessionKey(scope, "claude", outer.SessionID, cid)
	if err := store.SaveSession(oldKey, clientstate.SessionState{Scope: scope, Harness: "claude", SessionID: outer.SessionID, ClientID: cid, LastAdmissionMode: "auto", Registration: clientstate.Registration{RegistrationID: "AAAAAAAAAAAAAAAAAAAAAAAAAA", Secret: clientstate.NewSecret(), Name: "parent-owner", ParentPID: outer.PID, ProcessKey: "parent-process", Revision: 1}}); err != nil {
		t.Fatal("parent fixture failed")
	}
	auth := &scriptedDirect{}
	opts := durableOptions{state: store, scope: scope, device: &deviceAuthOptions{authorizer: auth}, parentProcessKey: func(context.Context, int) (string, error) { return "child-process", nil }}
	d, release, err := opts.loadSessionRegistration(inner)
	if err != nil {
		t.Fatal("child state failed")
	}
	defer release()
	b := &ingressBinding{mu: &sync.Mutex{}, identity: inner, durable: d}
	if err := opts.prepareConversationHandover(t.Context(), b, inner); err != nil || len(auth.requests) != 0 || d.registrationID != "" {
		t.Fatal("nested child attempted parent handover")
	}
}

func TestRound2SlowResumeIsBoundedAndReplaysOneRequest(t *testing.T) {
	b, opts := suspendClockBinding(t)
	opts.timeout = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var requests []broker.ResumeRequest
	waits := 0
	opts.resumeClock = devicePollClock{sleep: func(ctx context.Context, d time.Duration) error {
		waits++
		if waits == 2 {
			cancel()
		}
		return ctx.Err()
	}}
	target := round2CallFunc(func(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		if p.Name != "resume_session" {
			t.Fatal("slow resume created another registration")
		}
		raw, _ := json.Marshal(p.Arguments)
		var req broker.ResumeRequest
		if json.Unmarshal(raw, &req) != nil {
			t.Fatal("resume request decode failed")
		}
		requests = append(requests, req)
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Second {
			t.Fatal("upstream call has no bounded deadline")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if _, ok := resumeDurable(ctx, target, b, opts, ""); ok {
		t.Fatal("slow canceled resume became ready")
	}
	if len(requests) != 2 || !reflect.DeepEqual(requests[0], requests[1]) {
		t.Fatal("slow lost response changed resume identity, request or lease")
	}
}

type round2PausedUpstream struct {
	mu                 sync.Mutex
	oldLease, newLease string
	resumes            int
	messages           chan broker.Message
}

func (u *round2PausedUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	u.mu.Lock()
	switch p.Name {
	case "resume_session":
		raw, _ := json.Marshal(p.Arguments)
		var req broker.ResumeRequest
		_ = json.Unmarshal(raw, &req)
		u.resumes++
		u.newLease = req.ChannelID
		u.mu.Unlock()
		return &mcp.CallToolResult{StructuredContent: broker.RegisterResult{Session: broker.Session{RegistrationID: req.RegistrationID, Name: "suspend-clock", Revision: 2, Generation: 2}}}, nil
	case "renew_lease":
		raw, _ := json.Marshal(p.Arguments)
		var req broker.RenewRequest
		_ = json.Unmarshal(raw, &req)
		expired := req.ChannelID == u.oldLease
		u.mu.Unlock()
		code := ""
		if expired {
			code = broker.CodeInvalid
		}
		return &mcp.CallToolResult{StructuredContent: broker.RegisterResult{Code: code}}, nil
	case "wait_for_message":
		raw, _ := json.Marshal(p.Arguments)
		var req map[string]string
		_ = json.Unmarshal(raw, &req)
		current := u.newLease != "" && req["channelId"] == u.newLease
		u.mu.Unlock()
		if !current {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		select {
		case m := <-u.messages:
			return &mcp.CallToolResult{StructuredContent: m}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	default:
		u.mu.Unlock()
		return nil, errors.New("unexpected paused-client call")
	}
}
func TestRound2ContinuedClientResumesExpiredLeaseAndDeliversHeldOnce(t *testing.T) {
	b, opts := suspendClockBinding(t)
	b.identity.ChannelID = "expired-before-continue"
	b.started = true
	opts.heartbeat = time.Millisecond
	u := &round2PausedUpstream{oldLease: b.identity.ChannelID, messages: make(chan broker.Message, 1)}
	u.messages <- broker.Message{To: b.identity.Name, Body: "held-before-sleep", RegistrationID: b.durable.registrationID}
	delivered := make(chan broker.Message, 2)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runDurableReceiver(ctx, u, b, opts, func(_ context.Context, m broker.Message) error { delivered <- m; return nil })
	}()
	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("continued client did not deliver held envelope")
	}
	cancel()
	<-done
	u.mu.Lock()
	resumes := u.resumes
	u.mu.Unlock()
	if resumes != 1 {
		t.Fatal("continued client did not establish exactly one replacement lease")
	}
	select {
	case <-delivered:
		t.Fatal("held envelope delivered twice")
	default:
	}
}

func TestRound2ClaudeAndCodexShareDirectoryWithoutSharingRegistration(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	claudeID := claudeHandoverIdentity(1720, 21720)
	codexID := claudeID
	codexID.Harness = "codex"
	first := r.identityHarness(t, dir, claudeID, handoverOptions)
	first.attach(t)
	second := r.identityHarness(t, dir, codexID)
	second.attach(t)
	if first.session.RegistrationID == second.session.RegistrationID || first.session.Name == second.session.Name {
		t.Fatal("harnesses shared a registration")
	}
	manager := r.harness(t, t.TempDir(), 21721)
	manager.attach(t)
	for _, h := range []*detachedHarness{first, second} {
		if err := call(t.Context(), manager.client, "send_message", broker.Message{From: manager.session.Name, To: h.session.Name, Body: h.session.Harness}, nil); err != nil {
			t.Fatal("harness message enqueue failed")
		}
	}
	for _, h := range []*detachedHarness{first, second} {
		select {
		case m := <-h.received:
			if m.Body != h.session.Harness {
				t.Fatal("message crossed harnesses")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("harness did not receive")
		}
	}
}

// A model tool can be in flight while the follow serializer handles a changed
// conversation. Holding a read request must not hold that serializer or duplicate
// the envelope arriving across the handover.
type round2HeldTool struct {
	Upstream
	entered, release chan struct{}
	once             *atomic.Bool
}

func (u *round2HeldTool) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name == "list_sessions" && u.once.CompareAndSwap(false, true) {
		close(u.entered)
		select {
		case <-u.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return u.Upstream.CallTool(ctx, p)
}
func (u *round2HeldTool) Close() error { closeUpstream(u.Upstream); return nil }
func TestRound2ClearWithToolInFlightAndIncomingMessageIsOnce(t *testing.T) {
	r := newDetachedRuntime(t)
	initial := claudeHandoverIdentity(1730, 21730)
	roster := &followRoster{id: initial}
	entered, release := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	h := r.identityHarness(t, t.TempDir(), initial, func(o *durableOptions) {
		followOptions(roster)(o)
		original := o.device.newUpstream
		o.device.newUpstream = func(token string) Upstream {
			return &round2HeldTool{Upstream: original(token), entered: entered, release: release, once: &once}
		}
	})
	h.attach(t)
	registered := h.session
	toolDone := make(chan struct{})
	go func() {
		defer close(toolDone)
		_, _ = h.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not enter")
	}
	var releaseOnce sync.Once
	unlock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unlock()
	roster.set(claudeHandoverIdentity(1731, initial.PID))
	manager := r.harness(t, t.TempDir(), 21731)
	manager.attach(t)
	if err := call(t.Context(), manager.client, "send_message", broker.Message{From: manager.session.Name, To: registered.Name, Body: "clear-in-flight"}, nil); err != nil {
		t.Fatal("concurrent send failed")
	}
	select {
	case <-h.received:
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight tool blocked follow delivery")
	}
	status := h.status(t)
	if !status.Registered || status.RegistrationID != registered.RegistrationID || status.Name != registered.Name {
		t.Fatal("concurrent clear lost registration")
	}
	select {
	case <-h.received:
		t.Fatal("concurrent clear duplicated delivery")
	default:
	}
	// Close releases the old upstream; completing that old read may fail safely.
	// The tool completion is checked after releasing the synthetic stall below.
	unlock()
	select {
	case <-toolDone:
	case <-time.After(2 * time.Second):
		t.Fatal("old tool did not finish safely")
	}
}

func TestRound2AlreadyDequeuedMessageSurvivesLeaseRenewalFailure(t *testing.T) {
	b, opts := suspendClockBinding(t)
	opts.heartbeat = time.Millisecond
	entered := make(chan struct{})
	leaseCanceled := make(chan struct{})
	postAfterRenew := make(chan struct{})
	var cancelOnce sync.Once
	var waits atomic.Int32
	target := round2CallFunc(func(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		switch p.Name {
		case "wait_for_message":
			if waits.Add(1) > 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			context.AfterFunc(ctx, func() { cancelOnce.Do(func() { close(leaseCanceled) }) })
			return &mcp.CallToolResult{StructuredContent: broker.Message{To: b.identity.Name, RegistrationID: b.durable.registrationID, Body: "already-dequeued"}}, nil
		case "renew_lease":
			select {
			case <-entered:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &mcp.CallToolResult{StructuredContent: broker.RegisterResult{Code: broker.CodeInvalid}}, nil
		default:
			return nil, errors.New("unexpected expiry race tool")
		}
	})
	delivered := make(chan struct{}, 2)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan receiverExit, 1)
	go func() {
		done <- receiveDurableOnce(ctx, target, b, opts, func(postCtx context.Context, _ broker.Message) error {
			close(entered)
			<-postAfterRenew
			if err := postCtx.Err(); err != nil {
				return err
			}
			delivered <- struct{}{}
			return nil
		})
	}()
	// Force lease invalidation only after the native sink has the dequeued
	// envelope, and release the simulated socket write only after lease cancel.
	select {
	case <-leaseCanceled:
	case <-time.After(2 * time.Second):
		close(postAfterRenew)
		t.Fatal("expiry race did not cancel old receive stream")
	}
	close(postAfterRenew)
	select {
	case exit := <-done:
		if exit != exitUpstream {
			t.Fatal("renew failure lost recovery exit")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not join delivery")
	}
	if len(delivered) != 1 {
		t.Fatal("already dequeued message was lost when the lease context was canceled")
	}
}
