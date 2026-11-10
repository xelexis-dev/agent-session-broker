package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type ackSwapHost struct {
	mu   sync.Mutex
	seen map[string]int
}

func (h *ackSwapHost) accept(m broker.Message) { h.mu.Lock(); defer h.mu.Unlock(); h.seen[m.Body]++ }
func (h *ackSwapHost) counts() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	duplicates := 0
	for _, n := range h.seen {
		duplicates += n - 1
	}
	return len(h.seen), duplicates
}

type ackSwapGeneration struct {
	binding *ingressBinding
	router  *codexIngress
	cancel  context.CancelFunc
	done    chan struct{}
	unlock  func()
	writes  *hookWriteCompletions
}

func (g *ackSwapGeneration) stop() { g.cancel(); <-g.done; g.router.close(); g.unlock() }
func startAckSwapGeneration(t *testing.T, up *switchableUpstream, dir string, busy bool, push func(context.Context, broker.Message) error) *ackSwapGeneration {
	t.Helper()
	store, err := clientstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	opts := (durableOptions{enabled: true, state: store, scope: "http://broker/mcp?project=brk", project: "brk", heartbeat: time.Hour, timeout: time.Second, hookWrites: &hookWriteCompletions{}, backoff: func(int) time.Duration { return time.Millisecond }}).withDefaults()
	id := broker.AttachRequest{Harness: "codex", Name: "swap-client", SessionID: "11111111-1111-4111-8111-111111111111", PID: 4242, CWD: "/synthetic", ChannelID: "initial-lease"}
	b, unlock := ackClientBinding(t, opts, id)
	router := newCodexIngress(push)
	b.router = router
	if b.durable.registrationID == "" {
		if _, err := registerDurable(t.Context(), up, b, opts, id); err != nil {
			unlock()
			t.Fatal(err)
		}
	} else if _, ok := resumeDurable(t.Context(), up, b, opts, ""); !ok {
		unlock()
		t.Fatal("swap resume failed")
	}
	if bindingMessageAck(b) == nil {
		unlock()
		t.Fatal("swap harness failed to negotiate ack")
	}
	if busy {
		if _, err := writeCodexHook(router, "UserPromptSubmit"); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); receiveDurableOnce(ctx, up, b, opts, router.deliver) }()
	return &ackSwapGeneration{b, router, cancel, done, unlock, opts.hookWrites}
}
func (g *ackSwapGeneration) writeHook(t *testing.T, host *ackSwapHost) {
	t.Helper()
	out, done, failed, err := g.router.hookHandoff("PostToolUse")
	if err != nil {
		t.Fatal(err)
	}
	if done == nil {
		return
	}
	result := g.writes.result(done, failed)
	result.StructuredContent = out
	raw, _ := json.Marshal(result)
	wire := &hookWriteConnection{completions: g.writes, Connection: ackWriteProbe{write: func(_ context.Context, message jsonrpc.Message) error {
		response := message.(*jsonrpc.Response)
		var result struct {
			StructuredContent map[string]any `json:"structuredContent"`
		}
		if json.Unmarshal(response.Result, &result) != nil {
			return fmt.Errorf("invalid hook result")
		}
		hook, _ := result.StructuredContent["hookSpecificOutput"].(map[string]any)
		content, _ := hook["additionalContext"].(string)
		_, body, ok := strings.Cut(content, "\n")
		if !ok {
			return fmt.Errorf("missing host batch")
		}
		var messages []broker.Message
		if json.Unmarshal([]byte(body), &messages) != nil {
			return fmt.Errorf("invalid host batch")
		}
		for _, m := range messages {
			host.accept(m)
		}
		return nil
	}}}
	if err := wire.Write(t.Context(), &jsonrpc.Response{Result: raw}); err != nil {
		t.Fatal(err)
	}
}
func TestMessageAckSwapAcceptance(t *testing.T) {
	for _, busy := range []bool{true, false} {
		t.Run(map[bool]string{true: "busy-90-messages-5-swaps", false: "idle-90-swaps"}[busy], func(t *testing.T) {
			up := newSwitchableUpstream(t)
			dir := t.TempDir()
			host := &ackSwapHost{seen: map[string]int{}}
			accepted, refused := 0, 0
			send := func(body string) bool {
				var out broker.SendResult
				err := call(t.Context(), up, "send_message", broker.Message{From: "swap-client", To: "swap-client", Body: body}, &out)
				if err != nil || !out.Accepted {
					return false
				}
				accepted++
				return true
			}
			if busy {
				var retry []string
				for generation := 0; generation < 5; generation++ {
					g := startAckSwapGeneration(t, up, dir, true, func(_ context.Context, m broker.Message) error { host.accept(m); return nil })
					for i := 0; i < 18; i++ {
						body := fmt.Sprintf("busy-%d", generation*18+i)
						if !send(body) {
							refused++
							retry = append(retry, body)
						}
					}
					waitFor(t, "busy pending window", func() bool {
						g.router.mu.Lock()
						defer g.router.mu.Unlock()
						return len(g.router.pending) == min(accepted, 32)
					})
					g.stop()
				}
				g := startAckSwapGeneration(t, up, dir, true, func(_ context.Context, m broker.Message) error { host.accept(m); return nil })
				defer g.stop()
				deadline := time.Now().Add(8 * time.Second)
				for time.Now().Before(deadline) {
					g.writeHook(t, host)
					remaining := retry[:0]
					for _, body := range retry {
						if !send(body) {
							remaining = append(remaining, body)
						}
					}
					retry = remaining
					unique, _ := host.counts()
					if unique == 90 && len(retry) == 0 {
						break
					}
					time.Sleep(2 * time.Millisecond)
				}
			} else {
				for generation := 0; generation < 90; generation++ {
					body := fmt.Sprintf("idle-%d", generation)
					intercepted := make(chan struct{})
					g := startAckSwapGeneration(t, up, dir, false, func(ctx context.Context, m broker.Message) error {
						if m.Body == body {
							close(intercepted)
							<-ctx.Done()
							return ctx.Err()
						}
						host.accept(m)
						return nil
					})
					if !send(body) {
						g.stop()
						t.Fatal("idle message admission failed")
					}
					select {
					case <-intercepted:
					case <-time.After(time.Second):
						g.stop()
						t.Fatal("idle receiver did not intercept before queue handoff")
					}
					g.stop()
				}
				g := startAckSwapGeneration(t, up, dir, false, func(_ context.Context, m broker.Message) error { host.accept(m); return nil })
				defer g.stop()
				waitFor(t, "final idle replay", func() bool { unique, _ := host.counts(); return unique == 90 })
			}
			unique, duplicates := host.counts()
			lost := accepted - unique
			t.Logf("ACK_SWAP busy=%t accepted=%d delivered=%d lost=%d duplicates=%d initial-backpressure=%d", busy, accepted, unique, lost, duplicates, refused)
			if accepted != 90 || lost != 0 || duplicates != 0 {
				t.Fatal("ack swap acceptance failed")
			}
		})
	}
}
