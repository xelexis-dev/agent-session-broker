package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/memory"
)

// New central tools must be explicitly classified; unknown tools fail closed
// for authenticated callers, even when accidentally added to the server.
const credentialOnlyScope = "credential-only"

func toolScope(name string) string {
	switch name {
	case "report_bug":
		// Credential-only: handler checks an existing registration, no extra scope.
		return credentialOnlyScope
	case "list_sessions", "get_session":
		return access.SessionRead
	case "rebind_session", "attach_session", "register_session", "resume_session", "renew_lease", "probe_response", "report_ingress_failure":
		return access.SessionRegister
	case "rename_session":
		return access.SessionRename
	case "stop_session", "retire_session", "suspend_session", "unsuspend_session", "list_bug_reports", "get_bug_report", "set_bug_report_status":
		return access.SessionManage
	case "send_message":
		return access.MessageSend
	case "wait_for_message":
		return access.MessageReceive
	case "memory_get", "memory_search":
		return access.MemoryRead
	case "memory_put":
		return access.MemoryWrite
	default:
		return ""
	}
}

func scopeMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "tools/call" {
			scope := ""
			if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && params != nil {
				if isBugReportManagementTool(params.Name) {
					return bugReportManagementCall(ctx, next, method, req)
				}
				if params.Name == access.TokenInfoTool || toolScope(params.Name) == credentialOnlyScope {
					// The handler requires a valid credential, not an extra scope.
					return next(ctx, method, req)
				}
				scope = toolScope(params.Name)
			}
			if err := access.RequireScope(ctx, scope); err != nil {
				if err != access.ErrInsufficientScope {
					return nil, err
				}
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: access.ErrInsufficientScope.Error()}}}, nil
			}
		}
		return next(ctx, method, req)
	}
}

// This boundary also protects custom MemoryBackend implementations. The real
// memory store checks the same permission for callers that bypass MCP dispatch.
type scopedMemory struct{ MemoryBackend }

func (s scopedMemory) Put(ctx context.Context, in memory.PutInput) (memory.Node, error) {
	if err := access.RequireScope(ctx, access.MemoryWrite); err != nil {
		return memory.Node{}, err
	}
	return s.MemoryBackend.Put(ctx, in)
}
func (s scopedMemory) Get(ctx context.Context, id string) (memory.GetResult, error) {
	if err := access.RequireScope(ctx, access.MemoryRead); err != nil {
		return memory.GetResult{}, err
	}
	return s.MemoryBackend.Get(ctx, id)
}
func (s scopedMemory) Search(ctx context.Context, in memory.SearchInput) (memory.SearchResult, error) {
	if err := access.RequireScope(ctx, access.MemoryRead); err != nil {
		return memory.SearchResult{}, err
	}
	return s.MemoryBackend.Search(ctx, in)
}
