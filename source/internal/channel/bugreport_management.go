package channel

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"strings"
)

// Unknown tools on old servers have no fallback or retry, including for writes.
func unsupportedBugReportTool(name string, result *mcp.CallToolResult, err error) *mcp.CallToolResult {
	if name != "list_bug_reports" && name != "get_bug_report" && name != "set_bug_report_status" {
		return nil
	}
	text := ""
	if err != nil {
		text = err.Error()
	}
	if result != nil && result.IsError {
		for _, content := range result.Content {
			if item, ok := content.(*mcp.TextContent); ok {
				text += " " + item.Text
			}
		}
	}
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "unknown tool") && !strings.Contains(lower, "tool not found") {
		return nil
	}
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "unsupported: this broker does not provide " + name + " (requires server 0.17.3)"}}}
}
