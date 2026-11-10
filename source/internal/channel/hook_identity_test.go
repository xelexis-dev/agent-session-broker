package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const hookIdentityThread = "11111111-1111-4111-8111-111111111111"

type hookIdentityFixture struct {
	local   *mcp.ClientSession
	binding atomic.Pointer[ingressBinding]
	calls   atomic.Int64
	observe func(context.Context, string) (broker.AttachRequest, error)
}

func newHookIdentityFixture(t *testing.T, device bool) *hookIdentityFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	store, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	up := newSwitchableUpstream(t)
	f := &hookIdentityFixture{}
	id := broker.AttachRequest{Harness: "codex", SessionID: hookIdentityThread, PID: 4242, CWD: t.TempDir()}
	opts := durableOptions{enabled: true, tokenMode: true, harness: "codex", state: store, scope: "synthetic", project: "brk", heartbeat: time.Hour, timeout: time.Second, backoff: func(int) time.Duration { return time.Millisecond }, observe: func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return id, nil }, bindingCreated: func(b *ingressBinding) { f.binding.Store(b) }}
	var shared Upstream = up
	if device {
		clientID, err := store.ClientID()
		if err != nil {
			t.Fatal(err)
		}
		saved := clientstate.SessionState{Scope: opts.scope, Project: opts.project, Harness: id.Harness, SessionID: id.SessionID, ClientID: clientID, Registration: clientstate.Registration{Secret: strings.Repeat("s", 40)}, Credential: &clientstate.SessionCredential{AccessToken: "synthetic-credential", Nickname: "hook-bound"}}
		if err := store.SaveSession(clientstate.SessionKey(opts.scope, id.Harness, id.SessionID, clientID), saved); err != nil {
			t.Fatal(err)
		}
		opts.device = &deviceAuthOptions{newUpstream: func(string) Upstream { return up }}
		shared = unavailableUpstream{}
	}
	identify := func(ctx context.Context, req *mcp.CallToolRequest) (broker.AttachRequest, error) {
		if req == nil {
			return broker.AttachRequest{}, errors.New("no startup thread")
		}
		if req.Params.Name == "ingress_hook" {
			f.calls.Add(1)
		}
		thread, err := callerThreadID(req)
		if err != nil {
			return broker.AttachRequest{}, err
		}
		if req.Params.Name == "ingress_hook" {
			if f.observe != nil {
				return f.observe(ctx, thread)
			}
		}
		out := id
		out.SessionID = thread
		return out, nil
	}
	// Finish the unrelated startup prune before contending for fixture session locks.
	if _, err := store.Prune(time.Now()); err != nil {
		t.Fatal(err)
	}
	server, shutdown := newIngressServerWithOptions(ctx, shared, up.tools(), identify, func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
		r := newCodexIngress(func(context.Context, broker.Message) error { return nil })
		return r.deliver, r, nil
	}, opts)
	t.Cleanup(shutdown)
	t.Cleanup(cancel)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	f.local, err = mcp.NewClient(&mcp.Implementation{Name: "synthetic-hook", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.local.Close() })
	result, err := f.local.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Meta: mcp.Meta{"threadId": id.SessionID}, Arguments: map[string]any{"name": "hook-bound"}})
	if err != nil || result.IsError {

		for _, c := range result.Content {
			if text, ok := c.(*mcp.TextContent); ok {
				t.Log(text.Text)
			}
		}
		t.Fatal("fixture attach failed", err)
	}
	waitFor(t, "hook receiver ready", func() bool {
		b := f.binding.Load()
		if b == nil {
			return false
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		return receiverReadyLocked(b)
	})
	return f
}
func (f *hookIdentityFixture) hook(t *testing.T, event, thread, session string) *mcp.CallToolResult {
	t.Helper()
	p := &mcp.CallToolParams{Name: "ingress_hook", Arguments: map[string]any{"session_id": session, "hook_event_name": event, "turn_id": "turn"}}
	if thread != "" {
		p.Meta = mcp.Meta{"threadId": thread}
	}
	out, err := f.local.CallTool(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestHookIdentityBoundStacks(t *testing.T) {
	for _, device := range []bool{false, true} {
		t.Run(map[bool]string{false: "token", true: "device"}[device], func(t *testing.T) {
			f := newHookIdentityFixture(t, device)
			f.calls.Store(0)
			for i := 0; i < 10; i++ {
				event := "PreToolUse"
				if i%2 != 0 {
					event = "PostToolUse"
				}
				if f.hook(t, event, hookIdentityThread, hookIdentityThread).IsError {
					t.Fatal("hook rejected")
				}
			}
			if got := f.calls.Load(); got != 0 {
				t.Fatalf("10 hooks full identify calls=%d, want0", got)
			}
			b := f.binding.Load()
			b.router.mu.Lock()
			defer b.router.mu.Unlock()
			if b.router.hookSeq != 10 || b.router.activeTurn != "turn" || b.router.toolTurn != "" || !b.router.active {
				t.Fatal("hook state changed")
			}
		})
	}
}

func TestHookIdentityDiscoveryFences(t *testing.T) {
	for _, device := range []bool{false, true} {
		for _, kind := range []string{"claim-unconfirmed", "thread-unconfirmed", "holder-waiting", "suspended", "failed", "stale-lease", "stale-generation", "handed-over", "retired", "new", "resume", "fork", "thread-mismatch", "session-mismatch", "stale-env", "invalid-meta"} {
			t.Run(fmt.Sprintf("%v/%s", device, kind), func(t *testing.T) {
				f := newHookIdentityFixture(t, device)
				b := f.binding.Load()
				thread, session := hookIdentityThread, hookIdentityThread
				const other = "22222222-2222-4222-8222-222222222222"
				// Unknown threads must go through discovery, which rejects this synthetic unknown.
				f.observe = func(_ context.Context, thread string) (broker.AttachRequest, error) {
					if thread != hookIdentityThread {
						return broker.AttachRequest{}, errors.New("unknown synthetic thread")
					}
					b.mu.Lock()
					defer b.mu.Unlock()
					return b.identity, nil
				}
				b.mu.Lock()
				switch kind {
				case "claim-unconfirmed":
					b.claimThread = hookIdentityThread
					b.claimConfirmed = false
					thread = ""
				case "thread-unconfirmed":
					b.threadConfirmed = false
					thread = ""
				case "holder-waiting":
					b.holderWaiting = true
				case "suspended":
					b.resumeSuspended = true
				case "failed":
					b.failed = true
				case "stale-lease":
					b.ingressReadyChannel = "different"
				case "stale-generation":
					b.ingressReadyGeneration++
				case "handed-over":
					b.durable.handedOver = true
				case "retired":
					b.retired = true
				case "new", "resume", "fork":
					thread = other
					session = other
				case "thread-mismatch":
					thread = other
				case "session-mismatch":
					session = other
				case "stale-env":
					thread = ""
					session = other
				}
				b.mu.Unlock()
				t.Setenv("CODEX_THREAD_ID", hookIdentityThread)
				f.calls.Store(0)
				if kind == "invalid-meta" {
					_, err := f.local.CallTool(t.Context(), &mcp.CallToolParams{Name: "ingress_hook", Meta: mcp.Meta{"threadId": false}, Arguments: map[string]any{"session_id": session, "hook_event_name": "PreToolUse"}})
					if err != nil {
						t.Fatal(err)
					}
					// Claim handling rejects explicit invalid metadata before discovery, never env fallback.
				} else {
					f.hook(t, "PreToolUse", thread, session)
					if got := f.calls.Load(); got < 1 {
						t.Fatal("discovery bypassed")
					}
					if got := f.calls.Load(); got > 1 && kind != "thread-unconfirmed" {
						t.Fatalf("duplicate request observation: %d", got)
					}
				}
			})
		}
	}
}

func TestHookIdentityEnvFallbackAndClaimConfirmation(t *testing.T) {
	for _, device := range []bool{false, true} {
		t.Run(fmt.Sprint(device), func(t *testing.T) {
			f := newHookIdentityFixture(t, device)
			b := f.binding.Load()
			t.Setenv("CODEX_THREAD_ID", hookIdentityThread)
			if f.hook(t, "PreToolUse", "", hookIdentityThread).IsError {
				t.Fatal("confirmed env fallback rejected")
			}
			b.mu.Lock()
			b.claimThread = hookIdentityThread
			b.claimConfirmed = false
			b.mu.Unlock()
			f.calls.Store(0)
			if f.hook(t, "PostToolUse", hookIdentityThread, hookIdentityThread).IsError {
				t.Fatal("matching claim failed")
			}
			b.mu.Lock()
			confirmed := b.claimConfirmed
			b.mu.Unlock()
			if !confirmed || f.calls.Load() != 0 {
				t.Fatal("claim handling not completed before fast lookup")
			}
		})
	}
	for _, raw := range []any{false, "invalid", 42, nil} {
		req := deviceRequest(hookIdentityThread)
		req.Params.Meta["threadId"] = raw
		t.Setenv("CODEX_THREAD_ID", hookIdentityThread)
		if _, err := callerThreadID(req); err == nil {
			t.Fatal("invalid present meta fell back to env")
		}
	}
}

func TestHookIdentityReplacementInvalidatesObservation(t *testing.T) {
	var mu sync.Mutex
	old := heartbeatBinding()
	old.mu = &mu
	old.identity.Harness = "codex"
	old.identity.SessionID = hookIdentityThread
	old.threadConfirmed = true
	old.router = newCodexIngress(func(context.Context, broker.Message) error { return nil })
	old.started = true
	old.done = make(chan struct{})
	old.ingressReady = true
	old.ingressReadyChannel = old.identity.ChannelID
	old.ingressReadyGeneration = old.generation
	bindings := map[string]*ingressBinding{hookIdentityThread: old}
	req := deviceRequest(hookIdentityThread)
	req.Params.Name = "ingress_hook"
	req.Params.Arguments = json.RawMessage(`{"session_id":"` + hookIdentityThread + `","hook_event_name":"PreToolUse"}`)
	memo := &hookIdentityObservation{request: req}
	ctx := context.WithValue(t.Context(), hookIdentityKey{}, memo)
	var calls int
	full := func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
		calls++
		return bindings[hookIdentityThread].identity, nil
	}
	if _, err := observeHookIdentity(ctx, req, &mu, bindings, full); err != nil || calls != 0 {
		t.Fatal("bound fast lookup failed")
	}
	replacement := &ingressBinding{mu: &mu, identity: old.identity, durable: old.durable, router: old.router, threadConfirmed: true, started: true, done: make(chan struct{}), ingressReady: true, generation: old.generation + 1}
	replacement.identity.ChannelID = "new-lease"
	replacement.ingressReadyChannel = replacement.identity.ChannelID
	replacement.ingressReadyGeneration = replacement.generation
	bindings[hookIdentityThread] = replacement
	if memo.current(bindings[hookIdentityThread]) {
		t.Fatal("old observation accepted replacement pointer")
	}
	if _, err := observeHookIdentity(ctx, req, &mu, bindings, full); err != nil || calls != 1 {
		t.Fatal("replacement skipped full discovery")
	}
	replacement.generation++
	if memo.current(replacement) {
		t.Fatal("stale generation accepted before mutation")
	}
}

func TestHookIdentityRequiresLiveReadiness(t *testing.T) {
	for _, kind := range []string{"not-started", "no-durable", "no-router", "receiver-done", "receive-unavailable", "receive-reason", "ingress-pending"} {
		t.Run(kind, func(t *testing.T) {
			b := heartbeatBinding()
			b.identity.Harness = "codex"
			b.identity.SessionID = hookIdentityThread
			b.started = true
			b.threadConfirmed = true
			b.done = make(chan struct{})
			b.router = newCodexIngress(func(context.Context, broker.Message) error { return nil })
			b.ingressReady = true
			b.ingressReadyChannel = b.identity.ChannelID
			b.ingressReadyGeneration = b.generation
			b.mu.Lock()
			if !pinHookIdentity(b).eligible(hookIdentityThread, hookIdentityThread) {
				t.Fatal("fixture not ready")
			}
			switch kind {
			case "not-started":
				b.started = false
			case "no-durable":
				b.durable = nil
			case "no-router":
				b.router = nil
			case "receiver-done":
				close(b.done)
			case "receive-unavailable":
				b.receiveUnavailable = true
			case "receive-reason":
				b.receiveReason = "follow"
			case "ingress-pending":
				b.ingressReady = false
			}
			if pinHookIdentity(b).eligible(hookIdentityThread, hookIdentityThread) {
				t.Fatal("unready binding accepted")
			}
			b.mu.Unlock()
		})
	}
}

func TestHookIdentityInterruptReleasesBindingBeforeCompletion(t *testing.T) {
	for _, device := range []bool{false, true} {
		t.Run(fmt.Sprint(device), func(t *testing.T) {
			f := newHookIdentityFixture(t, device)
			b := f.binding.Load()
			if f.hook(t, "PreToolUse", hookIdentityThread, hookIdentityThread).IsError {
				t.Fatal("pre hook rejected")
			}
			var completed atomic.Bool
			b.router.mu.Lock()
			b.router.pending = []broker.Message{{Body: "synthetic", DeliverySequence: 1}}
			b.router.completions[1] = func() error {
				if !b.mu.TryLock() {
					return errors.New("completion called under binding mutex")
				}
				b.mu.Unlock()
				completed.Store(true)
				return nil
			}
			b.router.mu.Unlock()
			if f.hook(t, "Interrupt", hookIdentityThread, hookIdentityThread).IsError {
				t.Fatal("interrupt rejected")
			}
			b.router.mu.Lock()
			defer b.router.mu.Unlock()
			if !completed.Load() || b.router.hookSeq != 2 || b.router.active || b.router.activeTurn != "" || b.router.toolTurn != "" {
				t.Fatal("interrupt state or completion changed")
			}
		})
	}
}
