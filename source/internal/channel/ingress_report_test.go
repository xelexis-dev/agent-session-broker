package channel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestIngressReportAfterCancelledHTTPWait(t *testing.T) {
	entered := make(chan struct{})
	var reports atomic.Int32
	remote := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "0.17.3"}, nil)
	remote.AddTool(&mcp.Tool{Name: "wait_for_message", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	remote.AddTool(&mcp.Tool{Name: "report_ingress_failure", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reports.Add(1)
		return &mcp.CallToolResult{}, nil
	})
	central := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, nil))
	defer central.Close()
	up := newTestBrokerUpstream(t.Context(), central.URL)
	defer up.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = up.CallTool(ctx, &mcp.CallToolParams{Name: "wait_for_message", Arguments: map[string]any{}})
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("wait did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("wait did not stop")
	}
	reportIngressFailure(up, broker.AttachRequest{SessionID: claimUUID, ChannelID: "fixture-lease"}, "fixture-registration")
	if reports.Load() != 1 {
		t.Fatal("cancelled HTTP receiver prevented its lease release report")
	}
}

func TestIngressReportReconnectsClosedHTTPSession(t *testing.T) {
	var reports atomic.Int32
	remote := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "0.17.3"}, nil)
	remote.AddTool(&mcp.Tool{Name: "report_ingress_failure", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reports.Add(1)
		return &mcp.CallToolResult{}, nil
	})
	central := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, nil))
	defer central.Close()
	up := newTestBrokerUpstream(t.Context(), central.URL)
	defer up.Close()
	session, err := up.session(t.Context())
	if err != nil {
		t.Fatal("initial connection failed")
	}
	if err := session.Close(); err != nil {
		t.Fatal("fixture connection did not close")
	}
	reportIngressFailure(up, broker.AttachRequest{SessionID: claimUUID, ChannelID: "fixture-lease"}, "fixture-registration")
	if reports.Load() != 1 {
		t.Fatal("stale closed connection swallowed the lease release report")
	}
}

func TestIngressReportRetryBoundary(t *testing.T) {
	for _, kind := range []string{"closed", "closed-twice", "refusal", "other-error"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			var first context.Context
			up := round2CallFunc(func(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
				calls++
				if p.Name != "report_ingress_failure" {
					t.Fatal("wrong tool")
				}
				if calls == 1 {
					first = ctx
				} else if ctx != first {
					t.Fatal("retry reset the deadline budget")
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 3*time.Second {
					t.Fatal("missing total timeout")
				}
				switch kind {
				case "closed", "closed-twice":
					if calls == 1 || kind == "closed-twice" {
						return nil, fmt.Errorf("fixture: %w", mcp.ErrConnectionClosed)
					}
				case "refusal":
					return &mcp.CallToolResult{IsError: true}, nil
				case "other-error":
					return nil, errors.New("fixture transport error")
				}
				return &mcp.CallToolResult{}, nil
			})
			reportIngressFailure(up, broker.AttachRequest{SessionID: claimUUID, ChannelID: "fixture-lease"}, "fixture-registration")
			want := 1
			if kind == "closed" || kind == "closed-twice" {
				want = 2
			}
			if calls != want {
				t.Fatalf("calls=%d, want %d", calls, want)
			}
		})
	}
}
