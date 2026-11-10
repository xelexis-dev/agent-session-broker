package channel

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/nativeinbox"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func cloneHolderFixtureState(t *testing.T, source, target string) {
	t.Helper()
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(target, relative)
		if entry.IsDir() {
			return os.MkdirAll(dest, 0700)
		}
		if entry.Name() != "client-id" && !strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0600)
	})
	if err != nil {
		t.Fatal("synthetic state clone failed")
	}
}

func TestClonedClaudeStateShowsLiveHolderBeforeUnavailableInbox(t *testing.T) {
	r := newDetachedRuntime(t)
	source, clone := t.TempDir(), t.TempDir()
	ownerID := claudeHandoverIdentity(1821, 21821)
	ownerHolder := fixtureHolder()
	ownerHolder.PID = 101
	owner := r.identityHarness(t, source, ownerID, func(o *durableOptions) { o.receiverIdentity = fixtureReceiverIdentity{holder: ownerHolder} })
	owner.attach(t)
	before := owner.session
	_, _, originalSaved := handoverSaved(t, r, source, ownerID)
	cloneHolderFixtureState(t, source, clone)
	contenderID := ownerID
	contenderID.PID++
	contenderHolder := *ownerHolder
	contenderHolder.PID = 102
	waits := make(chan time.Duration, 8)
	advance := make(chan struct{})
	var sinkCalls atomic.Int32
	contender := r.identityHarness(t, clone, contenderID, func(o *durableOptions) {
		o.receiverIdentity = fixtureReceiverIdentity{holder: &contenderHolder}
		o.claudeInboxFactory = func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
			sinkCalls.Add(1)
			return nil, nil, nativeinbox.ErrUnavailable
		}
		o.resumeClock = devicePollClock{sleep: func(ctx context.Context, delay time.Duration) error {
			select {
			case waits <- delay:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case <-advance:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
	})
	select {
	case delay := <-waits:
		if delay != time.Second {
			t.Fatal("wrong local holder check interval")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cloned client never reached holder refusal")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	var status authStatus
	if err := call(ctx, contender.client, "auth_status", struct{}{}, &status); err != nil {
		t.Fatal("auth_status blocked behind live-holder wait instead of showing registration-in-use")
	}
	if status.Registered || status.Status != authAuthorized || !strings.Contains(status.Message, "registration-in-use") || strings.Contains(status.Message, "reconnect-required") || strings.Contains(status.Message, "/mcp") {
		t.Fatal("cloned client hid registration-in-use behind inbox recovery")
	}
	if status.Diagnostics == nil || status.Diagnostics.LastFailureCode != broker.CodeRegistrationInUse {
		t.Fatal("holder refusal diagnostic missing")
	}
	if sinkCalls.Load() != 0 {
		t.Fatal("native pairing ran before holder ownership")
	}
	resumes := r.resumes.Load()
	for _, seconds := range []int{5, 10, 20, 30, 30} {
		for range seconds {
			advance <- struct{}{}
			select {
			case delay := <-waits:
				if delay != time.Second {
					t.Fatal("same-device local holder check interval changed")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("holder retry stopped")
			}
		}
		resumes++
		if r.resumes.Load() != resumes {
			t.Fatal("local death checks changed remote resume backoff")
		}
	}

	store, key, saved := handoverSaved(t, r, clone, contenderID)
	if saved.Registration.RegistrationID != originalSaved.Registration.RegistrationID || saved.Registration.Secret != originalSaved.Registration.Secret || saved.Credential == nil || originalSaved.Credential == nil || saved.Credential.AccessToken != originalSaved.Credential.AccessToken {
		t.Fatal("waiting changed copied proof or credential")
	}
	diagnostic, err := store.LoadDiagnostics(key)
	if err != nil {
		t.Fatal("waiting diagnostics unavailable")
	}
	holderEvents := 0
	for _, event := range diagnostic.Events {
		if event.Reason == broker.CodeRegistrationInUse {
			holderEvents++
		}
	}
	if holderEvents != 1 {
		t.Fatal("holder retry flooded diagnostics")
	}
	var sessions []broker.Session
	if err := call(t.Context(), owner.client, "list_sessions", struct{}{}, &sessions); err != nil || len(sessions) != 1 {
		t.Fatal("waiting created another registration")
	}
	var current broker.Session
	if err := call(t.Context(), owner.client, "get_session", map[string]string{"name": before.RegistrationID}, &current); err != nil {
		t.Fatal(err)
	}
	if current.PID != ownerID.PID || current.Revision != before.Revision || current.RegistrationID != before.RegistrationID {
		t.Fatal("contender stole the live registration")
	}
	var sent broker.SendResult
	if err := call(t.Context(), owner.client, "send_message", broker.Message{From: before.Name, To: before.Name, Body: "single owner delivery"}, &sent); err != nil || !sent.Accepted {
		t.Fatal("live owner could not receive")
	}
	select {
	case message := <-owner.received:
		if message.Body != "single owner delivery" {
			t.Fatal("wrong owner message")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owner delivery stopped")
	}
	select {
	case <-contender.received:
		t.Fatal("contender received owner message")
	default:
	}
	if r.direct.Load() != 1 {
		t.Fatal("cloned client started fresh admission")
	}
}

type pausedHolderIdentity struct {
	fixtureReceiverIdentity
	entered, release chan struct{}
	once             sync.Once
}

func (p *pausedHolderIdentity) Current(ctx context.Context) *broker.ReceiverHolder {
	p.once.Do(func() { close(p.entered) })
	select {
	case <-p.release:
		return p.fixtureReceiverIdentity.Current(ctx)
	case <-ctx.Done():
		return nil
	}
}

func TestClonedClaudeStartupMonitorDoesNotMaskPendingHolderCheck(t *testing.T) {
	r := newDetachedRuntime(t)
	source, clone := t.TempDir(), t.TempDir()
	ownerID := claudeHandoverIdentity(1822, 21831)
	ownerHolder := fixtureHolder()
	ownerHolder.PID = 111
	owner := r.identityHarness(t, source, ownerID, func(o *durableOptions) { o.receiverIdentity = fixtureReceiverIdentity{holder: ownerHolder} })
	owner.attach(t)
	cloneHolderFixtureState(t, source, clone)
	nextID := ownerID
	nextID.PID++
	nextHolder := *ownerHolder
	nextHolder.PID = 112
	paused := &pausedHolderIdentity{fixtureReceiverIdentity: fixtureReceiverIdentity{holder: &nextHolder}, entered: make(chan struct{}), release: make(chan struct{})}
	defer close(paused.release)
	var options *durableOptions
	placeholder := broker.AttachRequest{Harness: "claude", PID: nextID.PID}
	r.identityHarness(t, clone, placeholder, func(o *durableOptions) {
		options = o
		o.receiverIdentity = paused
		handoverOptions(o)
		o.claudeInitial = func(context.Context) (broker.AttachRequest, error) { return nextID, nil }
		o.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return nextID, nil }
		o.claudeStartupClock = instantClaudeClock()
		o.claudeFollowClock = devicePollClock{now: time.Now, sleep: func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() }}
		o.claudeInboxFactory = func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
			return nil, nil, nativeinbox.ErrUnavailable
		}
	})
	select {
	case <-paused.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("startup resume never entered holder check")
	}
	// The first poll captured the unresolved startup placeholder before identity settled.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	f := options.claudeFollow
	if _, err := f.check(ctx, placeholder); err != nil {
		t.Fatal("startup monitor blocked pending holder check")
	}
	b := f.lookup(nextID.SessionID)
	if b == nil {
		t.Fatal("pending binding missing")
	}
	b.mu.Lock()
	reason := b.receiveReason
	b.mu.Unlock()
	if reason != "" || strings.Contains(f.summary(), "still-receiving") {
		t.Fatal("startup monitor mislabeled another live parent as native inbox unavailable before the holder refusal")
	}
}
