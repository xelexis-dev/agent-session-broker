package mcpserver

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func addTokenInfoTool(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{Name: access.TokenInfoTool, Description: "Read the calling credential's scope snapshot, admission mode and expiry."}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, access.TokenInfo, error) {
		if err := ctx.Err(); err != nil {
			return nil, access.TokenInfo{}, err
		}
		grant, ok := access.FromContext(ctx)
		if !ok || grant.TokenID == "" || !access.ValidScopes(grant.Scopes) || (!grant.ExpiresAt.IsZero() && !time.Now().Before(grant.ExpiresAt)) {
			return nil, access.TokenInfo{}, errors.New("credential-info-unavailable")
		}
		select {
		case <-grant.Revoked:
			return nil, access.TokenInfo{}, errors.New("credential-info-unavailable")
		default:
		}
		scopes := slices.Clone(grant.Scopes)
		slices.Sort(scopes)
		return nil, access.TokenInfo{ProjectGenerationID: grant.ProjectGenerationID, ProjectRefusals: grant.ProjectGenerationID != "", Scopes: scopes, AdmissionMode: grant.AdmissionMode, ExpiresAt: grant.ExpiresAt}, nil
	})
}
