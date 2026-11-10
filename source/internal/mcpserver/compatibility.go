package mcpserver

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type serverCompatibility = protocol.ServerCompatibility

func compatibilityMiddleware(version string, policy serverCompatibility) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/call" {
				var info *mcp.Implementation
				if r, ok := req.(*mcp.CallToolRequest); ok {
					info = r.ClientInfo()
				}
				client := ""
				if info != nil {
					client = info.Version
				}
				if !policy.Accepts(client) {
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: protocol.CompatibilityGuidance("client-incompatible", client, version, policy.MinClient)}}}, nil
				}
			}
			return next(ctx, method, req)
		}
	}
}
