package channel

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type oldRetireUpstream struct {
	calls     int
	toolError bool
}

func (u *oldRetireUpstream) CallTool(_ context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	u.calls++
	if p.Name != "retire_session" {
		return nil, errors.New("unexpected fallback")
	}
	if u.toolError {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "unknown tool: retire_session"}}}, nil
	}
	return nil, errors.New("method not found: retire_session")
}
func TestRetireOldServerFixedUnsupportedWithoutRetry(t *testing.T) {
	for _, toolError := range []bool{false, true} {
		t.Run(map[bool]string{false: "transport", true: "tool"}[toolError], func(t *testing.T) {
			u := &oldRetireUpstream{toolError: toolError}
			server := newClientToolServer(u, []*mcp.Tool{{Name: "retire_session", InputSchema: map[string]any{"type": "object"}}}, nil)
			st, ct := mcp.NewInMemoryTransports()
			ss, err := server.Connect(t.Context(), st, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ss.Close()
			cs, err := mcp.NewClient(&mcp.Implementation{Name: "retire-test", Version: "test"}, nil).Connect(t.Context(), ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()
			result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "retire_session", Arguments: map[string]string{"name": "old"}})
			if err != nil || !result.IsError || len(result.Content) != 1 {
				t.Fatal("missing safe error", err, result)
			}
			if text := result.Content[0].(*mcp.TextContent).Text; text != "unsupported: this broker does not provide retire_session (requires server 0.17.3)" {
				t.Fatal("wrong unsupported guidance", text)
			}
			if u.calls != 1 {
				t.Fatal("retire retried or fell back", u.calls)
			}
		})
	}
}

func (u *oldRetireUpstream) versionInfo() string { return "0.17.2" }
func TestRetireOldScopeMiddlewareDoesNotHideCurrentScopeDenial(t *testing.T) {
	result := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "insufficient_scope"}}}
	if !retirementUnsupported(&oldRetireUpstream{}, result, nil) {
		t.Fatal("old middleware denial not classified")
	}
	if retirementUnsupported(&currentRetireUpstream{}, result, nil) {
		t.Fatal("current server scope denial mislabeled")
	}
}

type currentRetireUpstream struct{ oldRetireUpstream }

func (*currentRetireUpstream) versionInfo() string { return "0.17.3" }
