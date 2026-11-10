package mcpserver

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"testing"
)

type senderBackend struct {
	stubBackend
	called bool
	status broker.SessionStatus
	from   string
}

func (b *senderBackend) GetSession(_ context.Context, name string) (broker.Session, error) {
	if name != "registered" {
		return broker.Session{}, broker.ErrSessionNotFound
	}
	return broker.Session{ID: "sender-id", Name: name, Status: b.status}, nil
}
func (b *senderBackend) SendMessage(_ context.Context, m broker.Message) (broker.SendResult, error) {
	b.called = true
	b.from = m.From
	return broker.SendResult{Accepted: true}, nil
}
func TestSendRequiresLiveRegisteredSender(t *testing.T) {
	for _, tc := range []struct {
		from     string
		status   broker.SessionStatus
		accepted bool
	}{{"unknown", broker.StatusIdle, false}, {"registered", broker.StatusMissing, false}, {"registered", broker.StatusStalled, false}, {"registered", broker.StatusIdle, true}} {
		t.Run(tc.from+string(tc.status), func(t *testing.T) {
			ctx := context.Background()
			b := &senderBackend{status: tc.status}
			st, ct := mcp.NewInMemoryTransports()
			ss, err := New(b, &stubMemory{}).Connect(ctx, st, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ss.Close()
			cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.17.3"}, nil).Connect(ctx, ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()
			result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"from": tc.from, "to": "target", "body": "nonce"}})
			if err != nil {
				t.Fatal(err)
			}
			if b.called != tc.accepted || result.IsError == tc.accepted {
				t.Fatalf("called=%v error=%v want accepted=%v", b.called, result.IsError, tc.accepted)
			}
		})
	}
}
