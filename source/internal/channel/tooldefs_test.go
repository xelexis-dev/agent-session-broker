package channel

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
)

// The forwarded tool definitions a client offers before it holds a session
// credential are generated from the remote project server, not hand-copied.
// Run with ASB_UPDATE_TOOLDEFS=1 to regenerate tooldefs.json after a server
// tool change; the plain run fails on any drift.
func TestForwardedToolSnapshotMatchesRemoteServer(t *testing.T) {
	registry, err := groups.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	factory := mcpserver.NewRemoteProjectFactory(registry)
	defer factory.Close()
	central := httptest.NewServer(mcpserver.ProjectHandler(factory))
	defer central.Close()
	ctx := context.Background()
	upstream, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.17.3"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: central.URL + "/mcp?project=brk", HTTPClient: central.Client()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	var live []*mcp.Tool
	cursor := ""
	for {
		listed, err := upstream.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		live = append(live, listed.Tools...)
		if cursor = listed.NextCursor; cursor == "" {
			break
		}
	}
	want, err := encodeToolSnapshot(live)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("ASB_UPDATE_TOOLDEFS") == "1" {
		if err := os.WriteFile("tooldefs.json", want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := encodeToolSnapshot(snapshotTools())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("tooldefs.json drifted from the remote server; regenerate with ASB_UPDATE_TOOLDEFS=1\nwant:\n%s\ngot:\n%s", want, got)
	}
	var names []string
	for _, tool := range snapshotTools() {
		names = append(names, tool.Name)
	}
	data, _ := json.Marshal(names)
	for _, required := range []string{"list_sessions", "get_session", "send_message", "stop_session", "suspend_session", "unsuspend_session", "memory_put", "memory_get", "memory_search", "list_bug_reports", "get_bug_report", "set_bug_report_status"} {
		if !hasTool(snapshotTools(), required) {
			t.Fatalf("snapshot lacks %s: %s", required, data)
		}
	}
	for _, internal := range []string{"register_session", "resume_session", "renew_lease", "probe_response", "report_ingress_failure"} {
		if hasTool(snapshotTools(), internal) {
			t.Fatalf("snapshot exposes internal tool %s", internal)
		}
	}
}

func TestProjectTokenClientListsManagementTools(t *testing.T) {
	f := newDeviceModeFixture(t, newDeviceAuthServer(t), newSwitchableUpstream(t), t.TempDir(), singleIdentity(), func(f *deviceModeFixture) { f.deferAttach = true })
	listed, err := f.local.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"suspend_session", "unsuspend_session", "list_bug_reports", "get_bug_report", "set_bug_report_status"} {
		if !hasTool(listed.Tools, name) {
			t.Fatalf("project-token client tools/list lacks %s", name)
		}
	}
	if f.codeRequests() != 0 || f.log.count("register_session") != 0 {
		t.Fatal("tool discovery initiated registration")
	}
}
