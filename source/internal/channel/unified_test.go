package channel

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

func TestSingleConnectionOffersRegistrationAndChannel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, ct := mcp.NewInMemoryTransports()
	up := &fakeUpstream{waiting: make(chan struct{}), registered: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- ServeDurableChannel(ctx, st, up, nil, broker.AttachRequest{SessionID: "uuid", PID: 123, CWD: t.TempDir()}, durableOptions{})
	}()
	c, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	list, err := c.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range list.Tools {
		names[tool.Name] = true
	}
	for _, name := range []string{"attach_session", "activate_channel", "reply"} {
		if !names[name] {
			t.Errorf("single connection missing %s", name)
		}
	}
	cancel()
	c.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("connection failed to stop")
	}
}
