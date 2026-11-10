package channel

import (
	_ "embed"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// tooldefs.json is generated from the remote project server's tools/list
// (see TestForwardedToolSnapshotMatchesRemoteServer). A project-token client
// serves these definitions before it holds a session credential, so the
// harness sees a complete tool list at initialize instead of an empty one.
//
//go:embed tooldefs.json
var toolSnapshotJSON []byte

func snapshotTools() []*mcp.Tool {
	var tools []*mcp.Tool
	if err := json.Unmarshal(toolSnapshotJSON, &tools); err != nil {
		return nil
	}
	return tools
}
