package serverruntime

// Opt-in matrix: only disposable servers and synthetic no-model Codex relays.
import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestRetireBinaryCompatibilityAndRollback(t *testing.T) {
	binaries := os.Getenv("ASB_RETIRE_BINARY_DIR")
	if binaries == "" {
		t.Skip("opt-in 0.17.2/current binary matrix")
	}
	dir := t.TempDir()
	store, err := controlpanel.OpenSQLite(filepath.Join(dir, "control"))
	if err != nil {
		t.Fatal("fixture store")
	}
	if store.Setup("synthetic-retire-password") != nil || store.CreateProject("test") != nil || store.SetInstanceHTTPPolicy("admin", true) != nil || store.SetScopeIssuanceReady("admin", true) != nil {
		t.Fatal("fixture policy")
	}
	parent, err := store.IssueProjectTokenWithOptionsFor("admin", "test", "fixture", controlpanel.ProjectTokenOptions{Scopes: phase6AutoScopes()})
	if err != nil {
		t.Fatal("fixture parent")
	}
	if err := store.Close(); err != nil {
		t.Fatal("fixture close")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	l.Close()
	server := &upgradeServer{t: t, address: address, dir: dir}
	server.start(filepath.Join(binaries, "bin-new-server"))
	t.Cleanup(server.stop)
	relay := phase6BuildRelay(t)
	t.Setenv("ASB_PHASE6_CLIENT", filepath.Join(binaries, "bin-old-client"))
	old := phase6Start(t, relay, "http://"+address, parent, "old", "", "11111111-1111-4111-8111-111111111111")
	out := old.call(t, "attach_session", struct{}{})
	if out.IsError {
		t.Fatal("0.17.2 attach refused")
	}
	old.registered(t)
	// The released client retains its fixed catalog after connecting to a new server.
	oldTools, err := old.session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range oldTools.Tools {
		if tool.Name == "retire_session" {
			t.Fatal("0.17.2 unexpectedly exposes retire")
		}
	}
	peer := phase6Start(t, relay, "http://"+address, parent, "peer", "", "22222222-2222-4222-8222-222222222222")
	if peer.call(t, "attach_session", struct{}{}).IsError {
		t.Fatal("peer attach refused")
	}
	peer.registered(t)
	target := phase6Decode[map[string]any](t, peer.call(t, "get_session", map[string]string{"name": peer.status(t)["name"].(string)}))
	t.Setenv("ASB_PHASE6_CLIENT", filepath.Join(binaries, "bin-new-client"))
	manager := phase6Start(t, relay, "http://"+address, parent, "manager", "", "44444444-4444-4444-8444-444444444444")
	if manager.call(t, "attach_session", struct{}{}).IsError {
		t.Fatal("new manager attach refused")
	}
	manager.registered(t)
	result := phase6Decode[protocol.RetireResult](t, manager.call(t, "retire_session", map[string]string{"registrationId": target["registrationId"].(string)}))
	if !result.OK || len(result.RevokedCredentials) == 0 {
		t.Fatal("new client did not retire target")
	}
	phase6Wait(t, "retired old client terminal", func() bool { return peer.status(t)["registered"] == false })
	// Confirm its old auto recovery does not create a new registration.
	rows := phase6Decode[[]map[string]any](t, old.call(t, "list_sessions", struct{}{}))
	if len(rows) != 2 {
		t.Fatal("retired auto client re-registered")
	}
	peer.close()
	old.close()
	manager.close()
	server.stop()
	// The old binary must load the same state, including detached+retired audit.
	server.start(filepath.Join(binaries, "bin-old-server"))
	t.Setenv("ASB_PHASE6_CLIENT", filepath.Join(binaries, "bin-new-client"))
	current := phase6Start(t, relay, "http://"+address, parent, "new", "", "33333333-3333-4333-8333-333333333333")
	if current.call(t, "attach_session", struct{}{}).IsError {
		t.Fatal("new client old server attach refused")
	}
	current.registered(t)
	listed, err := current.session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range listed.Tools {
		found = found || tool.Name == "retire_session"
	}
	if !found {
		t.Fatal("new snapshot lacks retire")
	}
	missing, err := current.session.CallTool(t.Context(), &mcp.CallToolParams{Name: "retire_session", Arguments: map[string]string{"name": "absent"}})
	if err != nil || !missing.IsError || len(missing.Content) != 1 {
		t.Fatal("old server response missing")
	}
	if missing.Content[0].(*mcp.TextContent).Text != "unsupported: this broker does not provide retire_session (requires server 0.17.3)" {
		t.Fatalf("old server guidance not fixed: %q", missing.Content[0].(*mcp.TextContent).Text)
	}
	t.Log("new server + actual 0.17.2 client fixed catalog, new client retire, old client terminal recovery, 0.17.2 rollback load, old server + new client: PASS")
}
