package channel

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type toolProfileKey struct{}

func clientToolProfile(ctx context.Context) string {
	if value, _ := ctx.Value(toolProfileKey{}).(string); value == "worker" {
		return value
	}
	return "full"
}

var workerHiddenTools = map[string]bool{"stop_session": true, "retire_session": true, "suspend_session": true, "unsuspend_session": true, "set_bug_report_status": true, "list_bug_reports": true, "get_bug_report": true}

func addToolProfileMiddleware(server *mcp.Server) {
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if clientToolProfile(ctx) != "worker" {
				return next(ctx, method, req)
			}
			if method == "tools/call" {
				if call, ok := req.(*mcp.CallToolRequest); ok && workerHiddenTools[call.Params.Name] {
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "tool-not-in-profile"}}, StructuredContent: map[string]any{"code": "tool-not-in-profile"}}, nil
				}
			}
			result, err := next(ctx, method, req)
			if method == "tools/list" && err == nil {
				if listed, ok := result.(*mcp.ListToolsResult); ok {
					out := *listed
					out.Tools = make([]*mcp.Tool, 0, len(listed.Tools))
					for _, tool := range listed.Tools {
						if !workerHiddenTools[tool.Name] {
							out.Tools = append(out.Tools, tool)
						}
					}
					return &out, nil
				}
			}
			return result, err
		}
	})
}
