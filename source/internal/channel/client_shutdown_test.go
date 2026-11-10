package channel

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type shutdownTool struct{ entered, canceled chan struct{} }

func (u *shutdownTool) CallTool(ctx context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	close(u.entered)
	<-ctx.Done()
	close(u.canceled)
	return nil, ctx.Err()
}

func TestClientShutdownCancelsHostToolWithoutHostDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u := &shutdownTool{make(chan struct{}), make(chan struct{})}
	identify := func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
		return broker.AttachRequest{}, nil
	}
	server, shutdown := newIngressServerWithOptions(ctx, u, []*mcp.Tool{{Name: "get_session", InputSchema: map[string]any{"type": "object"}}}, identify, nil, durableOptions{})
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	host, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	request, abort := context.WithCancel(context.Background())
	defer abort()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = host.CallTool(request, &mcp.CallToolParams{Name: "get_session", Arguments: map[string]any{}})
	}()
	select {
	case <-u.entered:
	case <-time.After(time.Second):
		t.Fatal("forwarded tool did not start")
	}
	cancel()
	defer shutdown()
	select {
	case <-u.canceled:
	case <-time.After(time.Second):
		abort()
		<-done
		t.Fatal("client shutdown did not cancel the host tool's upstream request")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled tool did not finish")
	}
}
