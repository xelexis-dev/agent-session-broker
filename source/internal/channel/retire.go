package channel

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"strings"
)

// Only known missing-tool responses become guidance; other failures stay errors.
func retirementUnsupported(upstream Upstream, result *mcp.CallToolResult, err error) bool {
	// Old scope middleware refuses unknown tools before tool lookup.
	if err == nil && insufficientScope(result) {
		if order, known := protocol.CompareVersions(upstreamVersion(upstream), "0.17.3"); known && order < 0 {
			return true
		}
	}

	missing := func(message string) bool {
		lower := strings.ToLower(message)
		return strings.Contains(lower, "unknown tool") || strings.Contains(lower, "tool not found") || strings.Contains(lower, "method not found")
	}
	if err != nil {
		return missing(err.Error())
	}
	if result != nil && result.IsError {
		for _, content := range result.Content {
			if t, ok := content.(*mcp.TextContent); ok && missing(t.Text) {
				return true
			}
		}
	}
	return false
}
