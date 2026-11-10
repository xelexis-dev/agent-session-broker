package mcpserver

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

// A selected-fields snapshot of the real remote catalog, with no credentials or
// sessions. Use the same opt-in helper on both sides of the removal.
func TestLegacyRemovalCatalogSnapshot(t *testing.T) {
	path := os.Getenv("ASB_LEGACY_CATALOG_SNAPSHOT")
	if path == "" {
		t.Skip("opt-in catalog evidence")
	}
	remote := broker.NewRemoteService()
	defer remote.Close()
	cs := catalogSession(t, New(remote, &stubMemory{}))
	listed, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	type toolSnapshot struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		InputSchema  any    `json:"inputSchema"`
		OutputSchema any    `json:"outputSchema"`
	}
	tools := make([]toolSnapshot, 0, len(listed.Tools))
	found := false
	for _, tool := range listed.Tools {
		tools = append(tools, toolSnapshot{tool.Name, tool.Description, tool.InputSchema, tool.OutputSchema})
		found = found || tool.Name == "spawn_session"
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	data, err := json.MarshalIndent(tools, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("remote catalog tools=%d spawn_session_listed=%t sha256=%x", len(tools), found, sha256.Sum256(append(data, '\n')))
}
