package channel

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type oldBugReportUpstream struct {
	calls int
	mode  string
}

func (u *oldBugReportUpstream) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	u.calls++
	if u.mode == "rpc" {
		return nil, errors.New("unknown tool: private-upstream-detail")
	}
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "tool not found: private-upstream-detail"}}}, nil
}
func TestBugReportManagementOldServerUnsupportedOnce(t *testing.T) {
	for _, mode := range []string{"rpc", "result"} {
		for _, name := range []string{"list_bug_reports", "get_bug_report", "set_bug_report_status"} {
			t.Run(mode+"/"+name, func(t *testing.T) {
				up := &oldBugReportUpstream{mode: mode}
				tool := &mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}
				client := catalogClient(t, newClientToolServer(up, []*mcp.Tool{tool}, nil))
				out, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
				want := fmt.Sprintf("unsupported: this broker does not provide %s (requires server 0.17.3)", name)
				if err != nil || !out.IsError || len(out.Content) != 1 || out.Content[0].(*mcp.TextContent).Text != want || up.calls != 1 {
					t.Fatal("unsupported response/retry", out, err, up.calls)
				}
			})
		}
	}
}
