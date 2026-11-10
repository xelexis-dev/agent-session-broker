package channel

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
)

// newSavedAutomaticFixture waits for the initial answer to be saved locally.
// A server-visible row alone is insufficient: stopping then may make the next
// client replay the first register request instead of exercising resume.
func newSavedAutomaticFixture(t *testing.T, upstream *switchableUpstream, stateDir string) *durableFixture {
	t.Helper()
	saved := make(chan struct{}, 1)
	f := newDurableFixtureWithPolicy(t, upstream, stateDir, true, func(opts *durableOptions, _ *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) {
		opts.bound = func(broker.Session) {
			select {
			case saved <- struct{}{}:
			default:
			}
		}
	})
	// A lost response deliberately blocks until caller cancellation. Keep this
	// explicit setup bounded; the client continues the persisted attempt itself.
	attachCtx, cancelAttach := context.WithTimeout(context.Background(), time.Second)
	_, _ = f.local.CallTool(attachCtx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
	cancelAttach()
	waitFor(t, "initial registration answer saved", func() bool {
		select {
		case <-saved:
			return true
		default:
			return false
		}
	})
	return f
}

func TestExplicitAttachRegistersAndReceives(t *testing.T) {
	upstream := newSwitchableUpstream(t)
	upstream.service.SetAdmission(grantAdmission{name: "automatic"})
	f := newDurableFixtureWithPolicy(t, upstream, t.TempDir(), true)
	if _, err := upstream.session("automatic"); err == nil {
		t.Fatal("initialization registered a fresh conversation")
	}
	f.attach(t, "")
	waitFor(t, "registration from explicit attach", func() bool {
		s, err := upstream.session("automatic")
		return err == nil && s.ID == f.identity.SessionID && s.AutomaticDeliveryReady
	})
	if err := call(context.Background(), upstream, "send_message", broker.Message{From: "automatic", To: "automatic", Body: "without attach"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-f.sink:
		if message.Body != "without attach" {
			t.Fatal(message)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("initialized registration has no receiver")
	}
}

func TestAutomaticMissingIdentityRetriesAreBounded(t *testing.T) {
	upstream := newSwitchableUpstream(t)
	var calls atomic.Int32
	f := newDurableFixtureWithPolicy(t, upstream, t.TempDir(), true, func(opts *durableOptions, identify *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) {
		opts.backoff = func(int) time.Duration { return time.Millisecond }
		*identify = func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
			calls.Add(1)
			return broker.AttachRequest{}, errors.New("identity unavailable")
		}
	})
	waitFor(t, "initial identity attempts", func() bool { return calls.Load() >= 6 })
	time.Sleep(30 * time.Millisecond)
	if calls.Load() != 6 {
		t.Fatalf("missing identity retried indefinitely: %d", calls.Load())
	}
	f.stop()
}

func TestAutomaticShutdownJoinsIdentityAndConcurrentManualAttach(t *testing.T) {
	upstream := newSwitchableUpstream(t)
	upstream.service.SetAdmission(grantAdmission{name: "automatic"})
	started := make(chan struct{})
	stateDir := t.TempDir()
	f := newDurableFixtureWithPolicy(t, upstream, stateDir, true, func(opts *durableOptions, identify *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) {
		original := *identify
		*identify = func(ctx context.Context, req *mcp.CallToolRequest) (broker.AttachRequest, error) {
			if req != nil {
				return original(ctx, req)
			}
			close(started)
			<-ctx.Done()
			return broker.AttachRequest{}, ctx.Err()
		}
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("automatic identity never started")
	}
	manual := make(chan struct{})
	go func() {
		defer close(manual)
		_, _ = f.local.CallTool(context.Background(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
	}()
	stopped := make(chan struct{})
	go func() { f.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not join bootstrap")
	}
	select {
	case <-manual:
	case <-time.After(time.Second):
		t.Fatal("manual attach remained blocked after shutdown")
	}
	next := newDurableFixtureWithPolicy(t, upstream, stateDir, true)
	next.attach(t, "")
	waitFor(t, "replacement acquired registration lock", func() bool { _, err := upstream.session("automatic"); return err == nil })
	next.stop()
}

func TestAutomaticReconnectResumesAndNeverRevivesDetached(t *testing.T) {
	upstream := newSwitchableUpstream(t)
	upstream.service.SetAdmission(grantAdmission{name: "automatic"})
	state := t.TempDir()
	first := newSavedAutomaticFixture(t, upstream, state)
	original, _ := upstream.session("automatic")
	first.stop()
	next := newDurableFixtureWithPolicy(t, upstream, state, true)
	waitFor(t, "automatic resume", func() bool {
		s, err := upstream.session("automatic")
		return err == nil && s.Revision == original.Revision+1 && s.AutomaticDeliveryReady
	})
	if err := call(context.Background(), upstream, "stop_session", map[string]string{"name": "automatic"}, nil); err != nil {
		t.Fatal(err)
	}
	next.stop()
	last := newDurableFixtureWithPolicy(t, upstream, state, true)
	time.Sleep(200 * time.Millisecond)
	s, err := upstream.session("automatic")
	if err != nil || s.ErrorCode != "detached" || s.AutomaticDeliveryReady {
		t.Fatalf("detached registration revived: %+v %v", s, err)
	}
	var status map[string]any
	if err := call(t.Context(), last.local, "auth_status", map[string]any{}, &status); err != nil {
		t.Fatal(err)
	}
	message, _ := status["message"].(string)
	if !strings.Contains(message, "registration-detached") || !strings.Contains(message, detachedGuidance) {
		t.Fatal("detached resume guidance missing")
	}
	last.stop()
}

func TestAutomaticResumeKeepsRequestAfterSlowLostAnswer(t *testing.T) {
	upstream := newSwitchableUpstream(t)
	upstream.service.SetAdmission(grantAdmission{name: "automatic"})
	// Make the server-visible/local-save gap deterministic before replacement.
	upstream.loseAtDeadline.Store("register_session")
	stateDir := t.TempDir()
	first := newSavedAutomaticFixture(t, upstream, stateDir)
	first.stop()
	upstream.loseAtDeadline.Store("resume_session")
	next := newDurableFixtureWithPolicy(t, upstream, stateDir, true, func(opts *durableOptions, _ *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) {
		opts.timeout = 20 * time.Millisecond
	})
	waitFor(t, "resume applied before answer loss", func() bool {
		s, err := upstream.session("automatic")
		return err == nil && s.Revision == 2 && s.AutomaticDeliveryReady
	})
	if err := call(context.Background(), upstream, "send_message", broker.Message{From: "automatic", To: "automatic", Body: "slow answer"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-next.sink:
	case <-time.After(time.Second):
		t.Fatal("slow lost resume answer fenced bootstrap's replacement request")
	}
	got, _ := upstream.session("automatic")
	if got.Revision != 2 {
		t.Fatal("bootstrap replaced its resume request after a timeout")
	}
}

func TestMetadataResumeKeepsRequestAfterOrdinaryToolCancellation(t *testing.T) {
	upstream := newSwitchableUpstream(t)
	upstream.service.SetAdmission(grantAdmission{name: "automatic"})
	stateDir := t.TempDir()
	saved := make(chan struct{}, 1)
	first := newDurableFixtureWithPolicy(t, upstream, stateDir, true, func(opts *durableOptions, _ *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) {
		opts.bound = func(broker.Session) {
			select {
			case saved <- struct{}{}:
			default:
			}
		}
	})
	first.attach(t, "")
	waitFor(t, "initial registration", func() bool { _, err := upstream.session("automatic"); return err == nil })
	// A broker row may become visible before the first client saves its
	// answer. Ensure the replacement exercises resume, not first-register replay.
	select {
	case <-saved:
	case <-time.After(time.Second):
		t.Fatal("initial registration answer was not saved")
	}
	first.stop()
	upstream.loseAtDeadline.Store("resume_session")
	upstream.appliedBeforeAnswerLoss = make(chan struct{})
	next := newDurableFixtureWithPolicy(t, upstream, stateDir, true, func(_ *durableOptions, identify *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) {
		original := *identify
		*identify = func(ctx context.Context, req *mcp.CallToolRequest) (broker.AttachRequest, error) {
			if req == nil {
				return broker.AttachRequest{}, errors.New("identity unavailable until ordinary tool call")
			}
			id, err := original(ctx, req)
			if err != nil {
				return id, err
			}
			id.SessionID, err = callerThreadID(req)
			return id, err
		}
	})
	params := &mcp.CallToolParams{Name: "get_session", Arguments: map[string]string{"name": "automatic"}, Meta: mcp.Meta{"threadId": next.identity.SessionID}}
	callCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type callOutcome struct {
		result *mcp.CallToolResult
		err    error
	}
	finished := make(chan callOutcome, 1)
	go func() {
		result, err := next.local.CallTool(callCtx, params)
		finished <- callOutcome{result, err}
	}()
	// Cancel only once the broker applied the resume and the shim withheld
	// its answer. A watchdog bounds a broken fixture, not the apply duration.
	appliedBeforeCancel := false
	select {
	case <-upstream.appliedBeforeAnswerLoss:
		appliedBeforeCancel = true
	case <-time.After(5 * time.Second):
	}
	cancel()
	var outcome callOutcome
	select {
	case outcome = <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("metadata tool call did not finish after cancellation")
	}
	if !appliedBeforeCancel {
		t.Fatal("timed out waiting for successful resume apply before answer loss")
	}
	firstResult, err := outcome.result, outcome.err
	if !errors.Is(err, context.Canceled) && !(err == nil && firstResult != nil && firstResult.IsError) {
		t.Fatalf("first metadata call did not return a cancellation error: transport=%v tool=%v", err, firstResult != nil && firstResult.IsError)
	}
	applied, err := upstream.session("automatic")
	if err != nil || applied.Revision != 2 {
		t.Fatalf("first metadata call did not apply resume before cancellation: revision=%d error=%v", applied.Revision, err)
	}
	if result, err := next.local.CallTool(context.Background(), params); err != nil || result.IsError {
		t.Fatalf("second metadata call: %+v %v", result, err)
	}
	if err := call(context.Background(), upstream, "send_message", broker.Message{From: "automatic", To: "automatic", Body: "after canceled tool"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-next.sink:
	case <-time.After(time.Second):
		t.Fatal("canceled metadata call discarded the applied resume request")
	}
	got, _ := upstream.session("automatic")
	if got.Revision != 2 {
		t.Fatalf("metadata retry replaced its applied lease: revision %d", got.Revision)
	}
}

func TestAutomaticRegistrationReplaysLostFirstAnswer(t *testing.T) {
	upstream := newSwitchableUpstream(t)
	upstream.service.SetAdmission(grantAdmission{name: "automatic"})
	upstream.loseNextAnswer("register_session")
	f := newDurableFixtureWithPolicy(t, upstream, t.TempDir(), true)
	f.attach(t, "")
	waitFor(t, "replayed first registration", func() bool { s, err := upstream.session("automatic"); return err == nil && s.AutomaticDeliveryReady })
	if err := call(context.Background(), upstream, "send_message", broker.Message{From: "automatic", To: "automatic", Body: "replayed"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.sink:
	case <-time.After(3 * time.Second):
		t.Fatal("first answer loss prevented receiver")
	}
	s, _ := upstream.session("automatic")
	if s.Revision != 1 {
		t.Fatalf("lost answer created another lease: %+v", s)
	}
}

func TestAutomaticFirstAnswerLossSurvivesClientProcessReplacement(t *testing.T) {
	upstream := newSwitchableUpstream(t)
	upstream.service.SetAdmission(grantAdmission{name: "automatic"})
	upstream.loseNextAnswer("register_session")
	stateDir := t.TempDir()
	first := newDurableFixtureWithPolicy(t, upstream, stateDir, true, func(opts *durableOptions, _ *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) {
		opts.backoff = func(int) time.Duration { return time.Hour }
	})
	first.attach(t, "")
	waitFor(t, "first request committed by broker", func() bool { _, err := upstream.session("automatic"); return err == nil })
	first.stop()
	key := clientstate.RegistrationKey("http://broker/mcp?project=brk", first.identity.Harness, first.identity.SessionID)
	pending, found, err := first.state.LoadRegistration(key)
	if err != nil || !found || pending.RegistrationID != "" || pending.PendingRequestID == "" || pending.PendingLease == "" {
		t.Fatal("lost first answer did not preserve its pending request")
	}
	next := newDurableFixtureWithPolicy(t, upstream, stateDir, true)
	// A deferred native sink must activate the replayed lease before it can
	// receive; mere server-side registration existence is not readiness.
	waitFor(t, "replacement activated replayed original lease", func() bool { s, e := upstream.session("automatic"); return e == nil && s.AutomaticDeliveryReady })
	if err := call(context.Background(), upstream, "send_message", broker.Message{From: "automatic", To: "automatic", Body: "after process replacement"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-next.sink:
	case <-time.After(3 * time.Second):
		t.Fatal("replacement did not replay pending registration")
	}
	got, _ := upstream.session("automatic")
	if got.Revision != 1 {
		t.Fatal("replacement issued a different first registration")
	}
}

func TestAutomaticReconnectNeverRecreatesDeletedRegistration(t *testing.T) {
	upstream := newSwitchableUpstream(t)
	upstream.service.SetAdmission(grantAdmission{name: "automatic"})
	stateDir := t.TempDir()
	first := newSavedAutomaticFixture(t, upstream, stateDir)
	// Deletion is an explicit administrative detach, never probe failure.
	if err := upstream.service.StopSession(context.Background(), "automatic"); err != nil {
		t.Fatal(err)
	}
	if err := upstream.service.RemoveDetachedSession(context.Background(), "automatic", nil); err != nil {
		t.Fatal(err)
	}
	first.stop()
	upstream.service.SetAdmission(grantAdmission{name: "automatic"})
	next := newDurableFixtureWithPolicy(t, upstream, stateDir, true)
	time.Sleep(200 * time.Millisecond)
	if _, err := upstream.session("automatic"); err == nil {
		t.Fatal("automatic reconnect recreated deleted record")
	}
	next.stop()
}

func TestMetadataObservationRequiresExplicitAttachForFreshConversation(t *testing.T) {
	upstream := newSwitchableUpstream(t)
	upstream.service.SetAdmission(grantAdmission{name: "automatic"})
	f := newDurableFixtureWithPolicy(t, upstream, t.TempDir(), true, func(opts *durableOptions, identify *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) {
		*identify = func(_ context.Context, req *mcp.CallToolRequest) (broker.AttachRequest, error) {
			if req == nil || req.Params == nil {
				return broker.AttachRequest{}, errors.New("identity unavailable")
			}
			thread, err := callerThreadID(req)
			if err != nil {
				return broker.AttachRequest{}, err
			}
			return broker.AttachRequest{Harness: "codex", SessionID: thread, PID: 4242, CWD: `C:\work\brk`}, nil
		}
	})
	time.Sleep(60 * time.Millisecond)
	if _, err := upstream.session("automatic"); err == nil {
		t.Fatal("registered without exact identity")
	}
	result, err := f.local.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_session", Arguments: map[string]string{"name": "automatic"}, Meta: mcp.Meta{"threadId": f.identity.SessionID}})
	if err != nil || !result.IsError {
		t.Fatal("ordinary tool registered its fresh caller")
	}
	explicit, err := f.local.CallTool(context.Background(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}, Meta: mcp.Meta{"threadId": f.identity.SessionID}})
	if err != nil || explicit.IsError {
		t.Fatal("explicit metadata attach failed")
	}
	wrong, err := f.local.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_sessions", Meta: mcp.Meta{"threadId": "22222222-2222-4222-8222-222222222222"}, Arguments: map[string]any{}})
	if err == nil && !wrong.IsError {
		t.Fatal("different conversation reused bound client")
	}
}

func TestClientRequiresTokenBeforeConnecting(t *testing.T) {
	err := RunClient(context.Background(), []string{"--broker-url", "http://127.0.0.1:1/mcp?project=brk"})
	if err == nil || !strings.Contains(err.Error(), "--token") {
		t.Fatalf("missing token did not fail before connection: %v", err)
	}
}
