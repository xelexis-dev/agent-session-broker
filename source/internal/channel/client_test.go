package channel

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"testing"
	"time"
)

type clientTestUpstream struct{ request broker.AttachRequest }

type timeoutUpstream struct{}

func (timeoutUpstream) CallTool(ctx context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type delayedRenewal struct {
	calls   int
	renewed chan struct{}
}

func (u *delayedRenewal) CallTool(ctx context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	u.calls++
	if u.calls == 1 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if u.calls == 2 {
		close(u.renewed)
	}
	return &mcp.CallToolResult{}, nil
}
func TestHeartbeatSurvivesOneSlowInspection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	up := &delayedRenewal{renewed: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- maintainRegistration(ctx, up, broker.AttachRequest{}, time.Millisecond, 5*time.Millisecond)
	}()
	select {
	case <-up.renewed:
	case err := <-done:
		t.Fatalf("heartbeat stopped: %v", err)
	case <-ctx.Done():
		t.Fatal("heartbeat did not renew")
	}
	cancel()
	<-done
}

func TestBrokerCallPreservesDeadlineForHeartbeatRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := call(ctx, timeoutUpstream{}, "attach_session", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost deadline: %v", err)
	}
}

type arrayUpstream struct{}

func (arrayUpstream) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{StructuredContent: []broker.Session{{Name: "session-2"}}}, nil
}

func TestClientNormalizesArrayResultForStrictMCPClients(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := newClientToolServer(arrayUpstream{}, []*mcp.Tool{{Name: "list_sessions", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "array"}}}, nil)
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
	result, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
	if err != nil || result.IsError || result.StructuredContent != nil || len(result.Content) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var sessions []broker.Session
	if err := json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &sessions); err != nil || len(sessions) != 1 || sessions[0].Name != "session-2" {
		t.Fatalf("lost array: %+v %v", sessions, err)
	}
}

func TestCallerThreadMetadataOverridesInheritedEnvironment(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", "22222222-2222-4222-8222-222222222222")
	wanted := "11111111-1111-4111-8111-111111111111"
	id, err := callerThreadID(&mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Meta: mcp.Meta{"threadId": wanted}}})
	if err != nil || id != wanted {
		t.Fatalf("id=%s err=%v", id, err)
	}
	for _, value := range []any{"", 123, "invalid"} {
		if _, err := callerThreadID(&mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Meta: mcp.Meta{"threadId": value}}}); err == nil {
			t.Fatalf("invalid metadata fell back: %v", value)
		}
	}
}

func TestNearestAgentNeverSkipsInnerClaude(t *testing.T) {
	harness, pid, err := nearestAgent([]int{10, 20, 30}, func(pid int) (string, error) {
		if pid == 20 {
			return "claude", nil
		}
		if pid == 30 {
			t.Fatal("reached outer Codex")
			return "codex", nil
		}
		return "", nil
	})
	if err != nil || harness != "claude" || pid != 20 {
		t.Fatalf("%s %d %v", harness, pid, err)
	}
	_, _, err = nearestAgent([]int{20, 30}, func(pid int) (string, error) {
		if pid == 30 {
			t.Fatal("fell back after failed process observation")
		}
		return "", errors.New("unobservable process")
	})
	if err == nil {
		t.Fatal("accepted unknown nearer parent")
	}
}

func TestClientDoesNotForwardUnknownIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	up := &clientTestUpstream{}
	server := newClientToolServer(up, nil, func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
		return broker.AttachRequest{}, errors.New("unknown parent")
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
	result, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]string{"name": "test"}})
	if err != nil || !result.IsError || up.request.SessionID != "" {
		t.Fatalf("unknown parent forwarded: %+v,%v", result, err)
	}
}

func (f *clientTestUpstream) CallTool(_ context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	data, _ := json.Marshal(p.Arguments)
	_ = json.Unmarshal(data, &f.request)
	return &mcp.CallToolResult{StructuredContent: broker.Session{ID: f.request.SessionID, Name: f.request.Name, Harness: f.request.Harness}}, nil
}

func TestClientNameOnlyAttachBothHarnesses(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		t.Run(harness, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			up := &clientTestUpstream{}
			identity := broker.AttachRequest{Harness: harness, SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: t.TempDir()}
			server := newClientToolServer(up, nil, func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) { return identity, nil })
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
			listing, err := c.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(listing.Tools[0].InputSchema)
			var schema struct {
				Required   []string       `json:"required"`
				Properties map[string]any `json:"properties"`
			}
			_ = json.Unmarshal(data, &schema)
			if len(schema.Properties) != 1 || len(schema.Required) != 1 || schema.Required[0] != "name" {
				t.Fatalf("not name-only: %s", data)
			}
			result, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]string{"name": "test-agent-1"}})
			if err != nil || result.IsError {
				t.Fatalf("attach=%+v,%v", result, err)
			}
			if up.request.Name != "test-agent-1" || up.request.Harness != harness || up.request.PID != identity.PID || up.request.CWD != identity.CWD || up.request.SessionID != identity.SessionID {
				t.Fatalf("wrong bound identity: %+v", up.request)
			}
		})
	}
}
