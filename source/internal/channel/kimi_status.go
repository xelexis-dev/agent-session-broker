package channel

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"strings"
)

func identityFailureGuidance(err error) string {
	if errors.Is(err, errUnsupportedHarness) || errors.Is(err, errKimiUnknown) || errors.Is(err, errKimiReconnect) || errors.Is(err, errKimiServer) {
		return err.Error()
	}
	return "Current session identity or automatic registration is unavailable; verify this MCP client's parent connection."
}
func installKimiStatusMiddleware(server *mcp.Server, ctx context.Context, up Upstream, opts durableOptions) {
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(c context.Context, method string, req mcp.Request) (mcp.Result, error) {
			tool, ok := req.(*mcp.CallToolRequest)
			if method != "tools/call" || !ok || tool.Params.Name == "xbroker_docs" {
				return next(c, method, req)
			}
			problem := opts.identityFailure
			if opts.kimi != nil {
				problem = opts.kimi.statusError()
			}
			if problem != nil && (tool.Params.Name == "auth_status" || !errors.Is(problem, errKimiUnknown)) {
				if tool.Params.Name == "auth_status" {
					status := kimiIdentityStatus(ctx, up, problem)
					if opts.identityFailure != nil {
						status.ParentDiagnostic = opts.parentDiagnostic
					}
					data, _ := json.Marshal(status)
					var fields map[string]any
					_ = json.Unmarshal(data, &fields)
					if opts.kimi != nil {
						fields["delivery"] = "turn-boundary"
						fields["pendingMessages"] = opts.kimi.queue.pendingCount()
					}
					data, _ = json.Marshal(fields)
					return &mcp.CallToolResult{StructuredContent: fields, Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil
				}
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: problem.Error()}}}, nil
			}
			result, err := next(c, method, req)
			if opts.kimi != nil && tool.Params.Name == "auth_status" && err == nil {
				if out, ok := result.(*mcp.CallToolResult); ok && out != nil {
					if out.IsError {
						for _, content := range out.Content {
							if text, ok := content.(*mcp.TextContent); ok {
								for _, known := range []error{errKimiUnknown, errKimiReconnect, errKimiServer, errUnsupportedHarness} {
									if strings.Contains(text.Text, known.Error()) {
										out.IsError = false
										out.StructuredContent = kimiIdentityStatus(ctx, up, known)
									}
								}
							}
						}
					}
					data, _ := json.Marshal(out.StructuredContent)
					var fields map[string]any
					if json.Unmarshal(data, &fields) == nil && fields != nil {
						fields["delivery"] = "turn-boundary"
						fields["pendingMessages"] = opts.kimi.queue.pendingCount()
						if message, _ := fields["message"].(string); !strings.Contains(message, kimiDeliveryGuidance) {
							fields["message"] = strings.TrimSpace(message + " " + kimiDeliveryGuidance)
						}
						out.StructuredContent = fields
						data, _ = json.Marshal(fields)
						out.Content = []mcp.Content{&mcp.TextContent{Text: string(data)}}
					}
				}
			}
			return result, err
		}
	})
}
