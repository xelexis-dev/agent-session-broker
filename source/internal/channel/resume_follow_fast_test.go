package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func fastFollowFixture(t *testing.T) (*claudeFollow, *durableOptions, broker.AttachRequest, func(context.Context) error) {
	t.Helper()
	id := claudeHandoverIdentity(701, 9101)
	opts := &durableOptions{device: &deviceAuthOptions{}, parentProcessKey: func(context.Context, int) (string, error) { return "same", nil }, claudeStartupClock: instantClaudeClock()}
	opts.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return id, nil }
	resolve := claudeConversationIdentity(id, opts)
	b := &ingressBinding{mu: &sync.Mutex{}, identity: id, started: true, durable: &durableRegistration{session: &sessionAuth{state: clientstate.SessionState{LastAdmissionMode: "auto"}}}}
	opts.bindingCreated(b)
	f := opts.claudeFollow
	f.lookup = func(string) *ingressBinding { return b }
	f.move = func(_ context.Context, _, next broker.AttachRequest) (broker.AttachRequest, error) { return next, nil }
	return f, opts, id, func(ctx context.Context) error { _, err := resolve(ctx, nil); return err }
}
func writeFollowSession(t *testing.T, root string, id broker.AttachRequest) string {
	t.Helper()
	path := filepath.Join(root, "sessions", fmt.Sprintf("%d.json", id.PID))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"pid": id.PID, "kind": "interactive", "cwd": id.CWD, "sessionId": id.SessionID})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestResumeFollowFastUnchangedToolCallsAvoidObserve(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	_, opts, id, resolve := fastFollowFixture(t)
	writeFollowSession(t, root, id)
	var calls atomic.Int32
	opts.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) {
		calls.Add(1)
		return id, nil
	}
	for i := 0; i < 10; i++ {
		if err := resolve(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("unchanged tool calls invoked observe %d times; want 0", got)
	}
}
func TestResumeFollowFastParallelToolCalls(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	_, opts, id, resolve := fastFollowFixture(t)
	writeFollowSession(t, root, id)
	var calls atomic.Int32
	opts.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) {
		calls.Add(1)
		time.Sleep(200 * time.Millisecond)
		return id, nil
	}
	began := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = resolve(t.Context()) }()
	}
	wg.Wait()
	if elapsed := time.Since(began); elapsed >= 200*time.Millisecond || calls.Load() != 0 {
		t.Fatalf("parallel calls waited for CLI: elapsed=%v observe=%d", elapsed, calls.Load())
	}
}

func TestResumeFollowFastChangedFileKeepsFullChecks(t *testing.T) {
	for _, changedKey := range []bool{false, true} {
		t.Run(fmt.Sprint(changedKey), func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", root)
			f, opts, id, _ := fastFollowFixture(t)
			next := claudeHandoverIdentity(702, id.PID)
			writeFollowSession(t, root, next)
			calls, moves, keys := 0, 0, 0
			opts.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { calls++; return next, nil }
			clock := instantClaudeClock()
			began := clock.now()
			opts.claudeStartupClock = clock
			opts.parentProcessKey = func(context.Context, int) (string, error) {
				keys++
				if changedKey && keys > 1 {
					return "changed", nil
				}
				return "same", nil
			}
			f.move = func(_ context.Context, _, to broker.AttachRequest) (broker.AttachRequest, error) {
				moves++
				return to, nil
			}
			got, err := f.check(t.Context(), id)
			if err != nil || calls < 8 || clock.now().Sub(began) < 750*time.Millisecond || keys != 2 {
				t.Fatalf("full checks lost: calls=%d keys=%d elapsed=%v err=%v", calls, keys, clock.now().Sub(began), err)
			}
			if changedKey {
				if moves != 0 || got.SessionID != id.SessionID {
					t.Fatal("changed process followed")
				}
			} else if moves != 1 || got.SessionID != next.SessionID {
				t.Fatal("changed session did not follow")
			}
		})
	}
}

func TestResumeFollowFastInvalidFileFallsBack(t *testing.T) {
	for _, kind := range []string{"missing", "corrupt", "pid", "kind", "cwd", "empty-session", "oversized", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", root)
			_, opts, id, resolve := fastFollowFixture(t)
			path := writeFollowSession(t, root, id)
			entry := map[string]any{"pid": id.PID, "kind": "interactive", "cwd": id.CWD, "sessionId": id.SessionID}
			switch kind {
			case "pid":
				entry["pid"] = id.PID + 1
			case "kind":
				entry["kind"] = "worker"
			case "cwd":
				entry["cwd"] = "/other"
			case "empty-session":
				entry["sessionId"] = ""
			}
			data, _ := json.Marshal(entry)
			switch kind {
			case "corrupt":
				data = []byte("{")
			case "oversized":
				data = append(data, []byte(strings.Repeat(" ", claudeSessionFileLimit))...)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "missing" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "unreadable" {
				opts.claudeSessionFile = func(int) ([]byte, error) { return nil, os.ErrPermission }
			}
			calls := 0
			opts.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { calls++; return id, nil }
			if err := resolve(t.Context()); err != nil || calls != 1 {
				t.Fatalf("fallback missing: observe=%d err=%v", calls, err)
			}
		})
	}
}

func TestResumeFollowFastStartupRetryDue(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	f, opts, id, _ := fastFollowFixture(t)
	writeFollowSession(t, root, id)
	f.lookup = func(string) *ingressBinding { return nil }
	f.noRegistered = func() bool { return true }
	now := time.Unix(1000, 0)
	opts.claudeFollowClock = devicePollClock{now: func() time.Time { return now }, sleep: func(context.Context, time.Duration) error { return nil }}
	f.current = id
	f.retryID = id.SessionID
	f.retryAt = now.Add(30 * time.Second)
	calls, attempts := 0, 0
	opts.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { calls++; return id, nil }
	f.startup = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) {
		attempts++
		return id, broker.HandoverRefusal{Reason: "suspended"}
	}
	for i := 0; i < 15; i++ {
		_, _ = f.check(t.Context(), id)
		now = now.Add(2 * time.Second)
	}
	if calls != 0 || attempts != 0 {
		t.Fatal("refused X retried before 30 seconds")
	}
	_, _ = f.check(t.Context(), id)
	if calls < 8 || attempts != 1 {
		t.Fatal("file fast path suppressed due 30-second retry")
	}
	calls = 0
	now = now.Add(2 * time.Second)
	_, _ = f.check(t.Context(), id)
	if calls != 0 || attempts != 1 {
		t.Fatal("refused retry repeated every two seconds")
	}
}

func TestResumeFollowFastDeliveryAvoidsObserve(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	f, opts, id, _ := fastFollowFixture(t)
	writeFollowSession(t, root, id)
	opts.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) {
		t.Fatal("delivery invoked CLI for unchanged conversation")
		return id, nil
	}
	b := f.lookup(id.SessionID)
	delivered := 0
	sink := f.wrapSink(b, id, func(context.Context, broker.Message) error { delivered++; return nil })
	for i := 0; i < 5; i++ {
		if err := sink(t.Context(), broker.Message{}); err != nil {
			t.Fatal(err)
		}
	}
	if delivered != 5 {
		t.Fatal("delivery lost")
	}
}
func TestReadClaudeSessionFileDefaultConfigAndCap(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	_, _, id, _ := fastFollowFixture(t)
	path := writeFollowSession(t, filepath.Join(root, ".claude"), id)
	data, err := readClaudeSessionFile(id.PID)
	if err != nil || !json.Valid(data) {
		t.Fatal("default config not read")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 2*claudeSessionFileLimit)), 0600); err != nil {
		t.Fatal(err)
	}
	data, err = readClaudeSessionFile(id.PID)
	if err != nil || len(data) != claudeSessionFileLimit+1 {
		t.Fatal("reader not bounded")
	}
}

func TestResumeFollowFastDeliveryDuringTransitionDoesNotWait(t *testing.T) {
	f, opts, id, _ := fastFollowFixture(t)
	calls := 0
	opts.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { calls++; return id, nil }
	sink := f.wrapSink(f.lookup(id.SessionID), id, func(context.Context, broker.Message) error { return nil })
	f.mu.Lock()
	defer f.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- sink(t.Context(), broker.Message{}) }()
	select {
	case err := <-done:
		if err != nil || calls != 1 {
			t.Fatal("transition did not use nonblocking fallback")
		}
	case <-time.After(time.Second):
		t.Fatal("receiver waited for transition mutex")
	}
}
func TestResumeFollowFastMCPAuthStatus(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	r := newDetachedRuntime(t)
	id := claudeHandoverIdentity(710, 9201)
	writeFollowSession(t, root, id)
	roster := &followRoster{id: id}
	var calls atomic.Int32
	h := r.identityHarness(t, t.TempDir(), id, func(o *durableOptions) {
		followOptions(roster)(o)
		observe := o.observe
		o.observe = func(ctx context.Context, id broker.AttachRequest) (broker.AttachRequest, error) {
			calls.Add(1)
			return observe(ctx, id)
		}
	})
	h.attach(t)
	calls.Store(0)
	for i := 0; i < 10; i++ {
		if !h.status(t).Registered {
			t.Fatal("registered binding lost")
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("real auth_status calls invoked observe %d times", got)
	}
}

func TestResumeFollowFastAfterSuccessfulStartupRetry(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	f, opts, id, _ := fastFollowFixture(t)
	writeFollowSession(t, root, id)
	b := f.lookup(id.SessionID)
	b.started = false
	f.current = id
	f.retryID = id.SessionID
	f.retryAt = time.Now().Add(-time.Second)
	f.noRegistered = func() bool { return !b.started }
	f.startup = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) {
		b.started = true
		return id, nil
	}
	calls := 0
	opts.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { calls++; return id, nil }
	if _, err := f.check(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	before := calls
	for i := 0; i < 5; i++ {
		_, _ = f.check(t.Context(), id)
	}
	if calls != before {
		t.Fatal("completed startup retry kept forcing CLI observation")
	}
}
