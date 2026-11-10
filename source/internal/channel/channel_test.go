package channel

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

type fakeUpstream struct {
	mu         sync.Mutex
	calls      []string
	waiting    chan struct{}
	registered chan struct{}
}

func (f *fakeUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, p.Name)
	f.mu.Unlock()
	switch p.Name {
	case "list_sessions":
		if f.registered != nil {
			select {
			case <-f.registered:
			default:
				return &mcp.CallToolResult{StructuredContent: []broker.Session{}}, nil
			}
		}
		return &mcp.CallToolResult{StructuredContent: []broker.Session{{Name: "claude-current", ID: "uuid", Harness: "claude", PID: 123, ExternallyManaged: true}}}, nil
	case "attach_session":
		return &mcp.CallToolResult{StructuredContent: broker.Session{Name: "claude-current", ID: "uuid", Harness: "claude", ExternallyManaged: true}}, nil
	case "wait_for_message":
		select {
		case <-f.waiting:
			<-ctx.Done()
			return nil, ctx.Err()
		default:
			close(f.waiting)
			return &mcp.CallToolResult{StructuredContent: broker.Message{From: "codex-current", To: "claude-current", Body: "automatic-nonce", SentAt: time.Date(2026, 10, 18, 1, 2, 3, 456000000, time.UTC)}}, nil
		}
	default:
		return &mcp.CallToolResult{StructuredContent: map[string]any{"accepted": true}}, nil
	}
}

// The channel push metadata is shared by every Claude channel path. Keep the
// exact millisecond UTC form the legacy channel test pinned.
func TestMessageMetadataSentAtMillisecondUTC(t *testing.T) {
	meta := messageMetadata(broker.Message{From: "codex-current", To: "claude-current", Body: "automatic-nonce", SentAt: time.Date(2026, 10, 18, 1, 2, 3, 456000000, time.UTC)})
	if meta["sentAt"] != "2026-10-18T01:02:03.456Z" {
		t.Fatalf("channel metadata sentAt = %q", meta["sentAt"])
	}
}
