package channel

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestCodexStopMissingHeartbeatUsesHookTurnAndRecordsRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	upstream := newSwitchableUpstream(t)
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pushed := make(chan string, 2)
	var ended atomic.Bool
	router := newCodexIngress(func(_ context.Context, m broker.Message) error { pushed <- m.Body; return nil })
	router.probe = func(context.Context, string) bool { return ended.Load() }
	const sid = "11111111-1111-4111-8111-111111111111"
	identify := func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
		return broker.AttachRequest{Harness: "codex", SessionID: sid, PID: 123, CWD: "/fixture"}, nil
	}
	const scope = "http://broker/mcp?project=brk"
	server, shutdown := newIngressServerWithOptions(ctx, upstream, upstream.tools(), identify, func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
		return router.deliver, router, nil
	}, durableOptions{enabled: true, state: state, project: "brk", scope: scope, heartbeat: 20 * time.Millisecond, timeout: time.Second})
	defer func() { cancel(); shutdown() }()
	cs := catalogClient(t, server)
	result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]string{"name": "worker"}})
	if err != nil || result.IsError {
		t.Fatal("fixture attachment failed", err)
	}
	result, err = cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "ingress_hook", Meta: mcp.Meta{"threadId": sid}, Arguments: map[string]string{"session_id": sid, "hook_event_name": "UserPromptSubmit", "turn_id": "a"}})
	if err != nil || result.IsError {
		t.Fatal("hook rejected", err)
	}
	router.mu.Lock()
	turn := router.activeTurn
	router.mu.Unlock()
	if turn != "a" {
		t.Fatal("MCP hook discarded turn_id")
	}
	for _, body := range []string{"one", "two"} {
		if err := router.deliver(t.Context(), broker.Message{Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	ended.Store(true)
	for _, want := range []string{"one", "two"} {
		select {
		case got := <-pushed:
			if got != want {
				t.Fatal("heartbeat reordered pending")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("heartbeat did not flush missing Stop")
		}
	}
	key := clientstate.RegistrationKey(scope, "codex", sid)
	waitFor(t, "recovery diagnostic", func() bool {
		d, err := state.LoadDiagnostics(key)
		if err != nil {
			return false
		}
		for _, e := range d.Events {
			if e.Event == "codex-stop-missing" {
				return true
			}
		}
		return false
	})
	cancel()
	shutdown()
	router.mu.Lock()
	closed := router.closed
	router.mu.Unlock()
	if !closed {
		t.Fatal("receiver shutdown left router open")
	}
}
