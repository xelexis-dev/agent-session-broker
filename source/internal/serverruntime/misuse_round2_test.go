package serverruntime

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
	"github.com/xelexis-dev/agent-session-broker/source/internal/memory"
)

func TestMisuseD4RecreatedProjectRejectsOldAuthorityAndData(t *testing.T) {
	r, parent := newDeviceRuntime(t)
	request := protectedRequest(950, "old")
	oldCredential := r.authorize(parent, request, "old")
	old := managedTool[broker.RegisterResult](t, r.bound(oldCredential), "register_session", request)
	node := managedTool[memory.Node](t, r.bound(oldCredential), "memory_put", memory.PutInput{Title: "old project", Body: "private synthetic memory", CreatedBy: "old"})
	report := managedTool[struct{ ID string }](t, r.bound(oldCredential), "report_bug", bugreport.Input{Title: "old project", Summary: "private synthetic report"})
	deleted := r.browser(http.MethodPost, "/projects/delete", url.Values{"project": {"test"}, "confirmName": {"test"}})
	if deleted.Code != 303 {
		t.Fatal("project deletion failed")
	}
	if created := r.browser(http.MethodPost, "/projects/create", url.Values{"name": {"test"}}); created.Code != 303 {
		t.Fatal("project recreation failed")
	}
	if r.mcpStatus(oldCredential) != 401 {
		t.Fatal("old session credential reached recreated project")
	}
	status, _ := r.rest("/oauth/device_authorization?project=test", parent, url.Values{"client_id": {"asb-client-v1"}, "scope": {"asb:session"}, "harness": {request.Harness}, "session_id": {request.SessionID}, "asb_client_instance_id": {request.ClientID}, "request_id": {"old-parent"}})
	if status != http.StatusUnauthorized {
		t.Fatal("old project token was not rejected at authentication")
	}
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
	store, err := controlpanel.OpenSQLite(filepath.Join(r.dir, "control"))
	if err != nil {
		t.Fatal(err)
	}
	freshParent, err := store.IssueProjectTokenFor("admin", "test", "fresh")
	if err != nil {
		t.Fatal("fresh issuance failed")
	}
	store.Close()
	r.start()
	r.login(r.password)
	freshCredential := r.authorize(freshParent, request, "fresh")
	restored := managedTool[broker.RegisterResult](t, r.bound(freshCredential), "resume_session", broker.ResumeRequest{RegistrationID: old.Session.RegistrationID, Harness: request.Harness, SessionID: request.SessionID, PID: request.PID, CWD: request.CWD, ClientID: request.ClientID, Secret: request.Secret, ChannelID: "old-proof", RequestID: "old-resume", ExpectedRevision: old.Session.Revision})
	if restored.Code == "" {
		t.Fatal("old registration proof resumed in recreated project")
	}
	oldSecret := request.Secret
	request.Secret = "fresh-registration-secret-long-enough-for-test"
	fresh := managedTool[broker.RegisterResult](t, r.bound(freshCredential), "register_session", request)
	if fresh.Code != "" || fresh.Session.RegistrationID == old.Session.RegistrationID {
		t.Fatal("old registration carried over")
	}
	denied := managedTool[broker.RegisterResult](t, r.bound(freshCredential), "resume_session", broker.ResumeRequest{RegistrationID: fresh.Session.RegistrationID, Harness: request.Harness, SessionID: request.SessionID, PID: request.PID, CWD: request.CWD, ClientID: request.ClientID, Secret: oldSecret, ChannelID: "wrong-old-secret", RequestID: "old-secret-new-id", ExpectedRevision: fresh.Session.Revision})
	if denied.Code == "" {
		t.Fatal("old project secret worked against new registration")
	}
	rows := managedTool[memory.SearchResult](t, r.bound(freshCredential), "memory_search", memory.SearchInput{})
	if len(rows.Nodes) != 0 {
		t.Fatal("old memory carried over")
	}
	var response struct{ IsError bool }
	managedRPC(t, r.bound(freshCredential), "tools/call", map[string]any{"name": "memory_get", "arguments": map[string]any{"id": node.ID}}, &response)
	if !response.IsError {
		t.Fatal("old memory ID remains readable")
	}
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
	reports, err := bugreport.Open(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reports.Get(report.ID); err == nil {
		t.Fatal("old report remains readable")
	}
	_, total, err := reports.List(bugreport.Filter{}, 1)
	reports.Close()
	if err != nil || total != 0 {
		t.Fatal("old reports carried over")
	}
	// Reopen for the fixture's normal final cleanup.
	r.start()
}

func r2CorruptRegistrationFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	store, err := controlpanel.OpenSQLite(filepath.Join(root, "control"))
	if err != nil {
		t.Fatal(err)
	}
	if store.Setup("synthetic-round2-password") != nil {
		t.Fatal("setup failed")
	}
	for _, p := range []string{"bad", "test"} {
		if store.CreateProject(p) != nil {
			t.Fatal("create failed")
		}
	}
	healthy, err := store.IssueToken("test", "healthy")
	if err != nil {
		t.Fatal("token fixture failed")
	}
	bad, err := store.IssueToken("bad", "broken")
	if err != nil {
		t.Fatal("token fixture failed")
	}
	store.Close()
	registry, err := groups.NewRegistry(filepath.Join(root, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	factory := mcpserver.NewManagedProjectFactory(registry, persistentBackend)
	for _, p := range []string{"bad", "test"} {
		if _, err := factory.RemoteBackend(t.Context(), p); err != nil {
			t.Fatal("project fixture failed")
		}
	}
	factory.Close()
	registry.Close()
	corrupt := filepath.Join(root, "projects", "p-626164", "registrations", "registrations.db")
	if os.WriteFile(corrupt, []byte("synthetic damaged SQLite header"), 0600) != nil {
		t.Fatal("corrupt fixture failed")
	}
	return root, healthy, bad
}

func TestMisuseD5CorruptProjectDatabaseDoesNotPreventHealthyStartup(t *testing.T) {
	root, healthy, bad := r2CorruptRegistrationFixture(t)
	handler, closeHandler, err := newManagedHandler(root, nil)
	if err != nil {
		t.Fatal("one corrupt project database blocked the entire server startup")
	}
	defer closeHandler()
	request := protectedRequest(951, "healthy")
	bound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+healthy)
		handler.ServeHTTP(w, r)
	})
	registered := managedTool[broker.RegisterResult](t, bound, "register_session", request)
	if registered.Code != "" {
		t.Fatal("healthy project registration failed")
	}
	rpc, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "list_sessions", "arguments": map[string]any{}}})
	req := httptest.NewRequest("POST", "https://broker/mcp?project=bad", bytes.NewReader(rpc))
	req.Header.Set("Authorization", "Bearer "+bad)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	if out.Code != 503 {
		t.Fatal("corrupt project did not return an availability error")
	}
}

func TestMisuseD5SharedBugReportDatabaseCorruptionDegradesFeature(t *testing.T) {
	root := t.TempDir()
	if os.MkdirAll(filepath.Join(root, "bugreports"), 0700) != nil {
		t.Fatal("fixture failed")
	}
	path := filepath.Join(root, "bugreports", "bugreports.db")
	original := []byte("synthetic damaged shared report database")
	if os.WriteFile(path, original, 0600) != nil {
		t.Fatal("fixture failed")
	}
	_, closeHandler, err := newManagedHandler(root, nil)
	if err != nil {
		t.Fatal("shared report corruption prevented degraded startup")
	}
	closeHandler()
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, original) {
		t.Fatal("corrupt shared database overwritten")
	}
	// Verified corruption degrades reporting without changing the corrupt bytes.
}

func TestMisuseD7SecondServerRefusesSameDataDirectory(t *testing.T) {
	root := t.TempDir()
	_, closeFirst, err := newManagedHandler(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, closeSecond, err := newManagedHandler(root, nil)
	if err == nil {
		closeSecond()
		closeFirst()
		t.Fatal("two server owners accepted same directory")
	}
	if err = closeFirst(); err != nil {
		t.Fatal(err)
	}
	_, closeThird, err := newManagedHandler(root, nil)
	if err != nil {
		t.Fatal("released directory lock remained held")
	}
	closeThird()
}
