package channel

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type ingressUpstream struct {
	input        chan broker.Message
	lease        chan string
	generation   atomic.Uint64
	failed       chan string
	waitDeadline chan bool
}

func (u *ingressUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	switch p.Name {
	case "attach_session":
		raw, _ := json.Marshal(p.Arguments)
		var r broker.AttachRequest
		json.Unmarshal(raw, &r)
		select {
		case u.lease <- r.ChannelID:
		default:
		}
		return &mcp.CallToolResult{StructuredContent: broker.Session{ID: r.SessionID, Name: r.Name, Harness: r.Harness, PID: r.PID, Generation: u.generation.Load(), AutomaticDeliveryReady: r.ChannelID != ""}}, nil
	case "report_ingress_failure":
		if u.failed != nil {
			u.failed <- "reported"
		}
		return &mcp.CallToolResult{}, nil
	case "wait_for_message":
		if u.waitDeadline != nil {
			_, bounded := ctx.Deadline()
			u.waitDeadline <- bounded
		}
		select {
		case m := <-u.input:
			return &mcp.CallToolResult{StructuredContent: m}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	default:
		return &mcp.CallToolResult{}, nil
	}
}

func TestIngressIdleWaitHasReceiverLifetime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := &ingressUpstream{input: make(chan broker.Message, 1), lease: make(chan string, 1), failed: make(chan string, 1), waitDeadline: make(chan bool, 2)}
	delivered := make(chan broker.Message, 1)
	done := make(chan struct{})
	go func() {
		receiveIngress(ctx, up, broker.AttachRequest{Name: "receiver"}, func(_ context.Context, m broker.Message) error { delivered <- m; return nil })
		close(done)
	}()
	select {
	case bounded := <-up.waitDeadline:
		if bounded {
			t.Fatal("idle wait has an artificial expiry that cancels the shared HTTP connection")
		}
	case <-time.After(time.Second):
		t.Fatal("receiver did not start")
	}
	up.input <- broker.Message{To: "receiver", Body: "idle delivery"}
	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Fatal("idle delivery missing")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("receiver ignored shutdown")
	}
}
func TestIngressClientNameRegistrationStartsReceiverWithoutActivation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	up := &ingressUpstream{input: make(chan broker.Message, 1), lease: make(chan string, 1)}
	delivered := make(chan broker.Message, 1)
	identify := func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
		return broker.AttachRequest{Harness: "claude", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: t.TempDir()}, nil
	}
	factory := func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
		return func(_ context.Context, m broker.Message) error { delivered <- m; return nil }, nil, nil
	}
	server := newIngressServer(ctx, up, nil, identify, factory)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	c, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	result, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]string{"name": "named"}})
	if err != nil || result.IsError {
		t.Fatalf("attach: %v %v", result, err)
	}
	select {
	case id := <-up.lease:
		if id == "" {
			t.Fatal("missing lease")
		}
	case <-ctx.Done():
		t.Fatal("not attached")
	}
	up.input <- broker.Message{From: "peer", To: "named", Body: "actual ingress"}
	select {
	case m := <-delivered:
		if m.Body != "actual ingress" {
			t.Fatal(m)
		}
	case <-ctx.Done():
		t.Fatal("receiver waited for channel activation")
	}
	result, err = c.CallTool(ctx, &mcp.CallToolParams{Name: "ingress_hook", Arguments: map[string]string{"session_id": "another", "hook_event_name": "Stop"}})
	if err != nil || result.IsError {
		t.Fatal("unregistered hook must be harmless before registration")
	}
}

func TestIngressHookRejectsOtherThreadAndRetiredGeneration(t *testing.T) {
	const owner = "11111111-1111-4111-8111-111111111111"
	const other = "22222222-2222-4222-8222-222222222222"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	up := &ingressUpstream{input: make(chan broker.Message), lease: make(chan string, 4), failed: make(chan string, 4)}
	up.generation.Store(1)
	cwd := t.TempDir()
	router := newCodexIngress(func(context.Context, broker.Message) error { return nil })
	identify := func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
		return broker.AttachRequest{Harness: "codex", SessionID: owner, PID: 123, CWD: cwd}, nil
	}
	server := newIngressServer(ctx, up, nil, identify, func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
		return router.deliver, router, nil
	})
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	c, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	attach := func() *mcp.CallToolResult {
		r, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]string{"name": "owner"}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := attach(); r.IsError {
		t.Fatal(r)
	}
	writeCodexHook(router, "UserPromptSubmit")
	if err := router.deliver(ctx, broker.Message{From: "peer", To: "owner", Body: "owner-only"}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"PostToolUse", "Stop", "Interrupt"} {
		r, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "ingress_hook", Meta: mcp.Meta{"threadId": other}, Arguments: map[string]string{"session_id": owner, "hook_event_name": event}})
		if err != nil || !r.IsError {
			t.Fatalf("foreign %s accepted: %v %v", event, r, err)
		}
	}
	r, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "ingress_hook", Meta: mcp.Meta{"threadId": owner}, Arguments: map[string]string{"session_id": owner, "hook_event_name": "PostToolUse"}})
	if err != nil || r.IsError {
		t.Fatalf("owner hook: %v %v", r, err)
	}
	data, _ := json.Marshal(r.StructuredContent)
	if !strings.Contains(string(data), "owner-only") {
		t.Fatalf("owner message was drained: %s", data)
	}
	// Simulate detach/re-attach even before the old wait goroutine reports exit.
	up.generation.Store(2)
	if r := attach(); !r.IsError {
		t.Fatal("retired generation advertised ready without a receiver")
	}
	select {
	case <-up.failed:
	case <-ctx.Done():
		t.Fatal("new lease not marked failed")
	}
	for attach().IsError {
		select {
		case <-ctx.Done():
			t.Fatal("fresh registration remained blocked after the retired receiver stopped")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
