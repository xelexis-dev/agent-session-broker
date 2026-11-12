package mcpserver

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func projectCapabilityMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, req)
		if err != nil {
			return result, err
		}
		grant, ok := access.FromContext(ctx)
		if !ok || (grant.ProjectGenerationID == "" && !access.ValidScopes(grant.Scopes)) {
			return result, nil
		}
		copyCaps := func(original *mcp.ServerCapabilities) *mcp.ServerCapabilities {
			if original == nil {
				return nil
			}
			out := *original
			out.Experimental = map[string]any{}
			for k, v := range original.Experimental {
				out.Experimental[k] = v
			}
			if grant.ProjectGenerationID != "" {
				out.Experimental[protocol.ProjectRefusalCapability] = map[string]any{"projectGenerationId": grant.ProjectGenerationID}
			}
			if scopes := credentialScopes(ctx); scopes != nil {
				out.Experimental[protocol.CredentialScopesCapability] = map[string]any{"scopes": scopes}
			}
			return &out
		}
		switch v := result.(type) {
		case *mcp.InitializeResult:
			copy := *v
			copy.Capabilities = copyCaps(v.Capabilities)
			return &copy, nil
		case *mcp.DiscoverResult:
			copy := *v
			copy.Capabilities = copyCaps(v.Capabilities)
			return &copy, nil
		}
		return result, nil
	}
}

// Scope signals are server-owned snapshots of the authenticated request only.
func credentialScopes(ctx context.Context) []string {
	grant, ok := access.FromContext(ctx)
	if !ok || !access.ValidScopes(grant.Scopes) {
		return nil
	}
	return access.WireScopes(grant.Scopes)
}
func credentialScopesResult(ctx context.Context, v protocol.RegisterResult, err error) protocol.RegisterResult {
	v.CredentialScopes = nil
	if err == nil && v.Code == "" {
		v.CredentialScopes = credentialScopes(ctx)
	}
	return v
}

// Registration metadata comes from this server, never from backend-supplied snapshots or refusals.
func registrationResult(ctx context.Context, v protocol.RegisterResult, err error, serverVersion string) protocol.RegisterResult {
	v = credentialScopesResult(ctx, v, err)
	v.ServerVersion = ""
	if err == nil && v.Code == "" {
		v.ServerVersion = serverVersion
	}
	return v
}
