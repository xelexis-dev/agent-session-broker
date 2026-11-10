package channel

import (
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"sort"
)

// encodeToolSnapshot normalizes a tool list for comparison and storage:
// internal registration tools are dropped and the rest is sorted by name.
func encodeToolSnapshot(tools []*mcp.Tool) ([]byte, error) {
	kept := make([]*mcp.Tool, 0, len(tools))
	for _, tool := range tools {
		if tool == nil || internalUpstreamTools[tool.Name] || tool.Name == "wait_for_message" {
			continue
		}
		kept = append(kept, tool)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Name < kept[j].Name })
	return json.MarshalIndent(kept, "", " ")
}
