package serverruntime

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/memory"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

func TestScopeUpgradeKeepsExistingRuntimeFeatures(t *testing.T) {
	r, parent := newDeviceRuntime(t)
	first := protectedRequest(11, "ignored")
	credential := r.authorize(parent, first, "session")
	registered := managedTool[broker.RegisterResult](t, r.bound(credential), "register_session", first)
	if registered.Code != "" {
		t.Fatal("initial registration failed")
	}
	if r.close() != nil {
		t.Fatal("fixture shutdown failed")
	}
	controlDir := filepath.Join(r.dir, "control")
	store, err := controlpanel.OpenSQLite(controlDir)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := store.IssueTokenFor("admin", "test", "legacy", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if store.Close() != nil {
		t.Fatal("fixture close failed")
	}
	db, err := sqlitestore.Open(controlDir, "control.db")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := db.Get("state")
	if err != nil {
		t.Fatal(err)
	}
	var old map[string]any
	if json.Unmarshal(raw, &old) != nil {
		t.Fatal("fixture decode failed")
	}
	old["version"] = 3
	for _, v := range old["projects"].(map[string]any) {
		p := v.(map[string]any)
		delete(p, "projectGenerationId")
		for _, v := range p["tokens"].([]any) {
			token := v.(map[string]any)
			delete(token, "scopeVersion")
			delete(token, "scopes")
			delete(token, "admissionMode")
			delete(token, "issuedBy")
			delete(token, "maxAutoBindings")
		}
		for _, v := range p["devices"].([]any) {
			d := v.(map[string]any)
			delete(d, "scopeVersion")
			delete(d, "scopes")
		}
	}
	raw, _ = json.Marshal(old)
	if db.Put("state", raw) != nil || db.Close() != nil {
		t.Fatal("historical DB seed failed")
	}
	r.start()
	r.login(r.password)
	// The same session credential and resume secret still address the same
	// persisted registration/revision, without issuing or approving a new code.
	got := managedTool[broker.Session](t, r.bound(credential), "get_session", map[string]string{"name": "session"})
	if got.RegistrationID != registered.Session.RegistrationID || got.Revision != registered.Session.Revision {
		t.Fatal("upgrade changed registration identity/revision")
	}
	resumed := managedTool[broker.RegisterResult](t, r.bound(credential), "resume_session", broker.ResumeRequest{RegistrationID: got.RegistrationID, Harness: first.Harness, SessionID: first.SessionID, PID: first.PID, CWD: first.CWD, ClientID: first.ClientID, ChannelID: "after-upgrade", Secret: first.Secret, RequestID: "after-upgrade", ExpectedRevision: got.Revision})
	if resumed.Code != "" {
		t.Fatal("original resume secret no longer works")
	}
	second := protectedRequest(12, "ignored")
	other := managedTool[broker.RegisterResult](t, r.bound(legacy), "register_session", second)
	if other.Code != "" || other.Session.Name != "legacy" {
		t.Fatal("same legacy credential no longer registers")
	}
	listed := managedTool[[]broker.Session](t, r.bound(credential), "list_sessions", map[string]any{})
	if len(listed) != 2 {
		t.Fatal("session listing lost registrations")
	}
	sent := managedTool[broker.SendResult](t, r.bound(credential), "send_message", broker.Message{From: "session", To: "legacy", Body: "scope-upgrade"})
	if !sent.Accepted {
		t.Fatal("message send lost")
	}
	message := managedTool[broker.Message](t, r.bound(legacy), "wait_for_message", map[string]string{"recipient": "legacy", "channelId": second.ChannelID})
	if message.Body != "scope-upgrade" {
		t.Fatal("message receive lost")
	}
	node := managedTool[memory.Node](t, r.bound(credential), "memory_put", memory.PutInput{Title: "scope upgrade", Body: "synthetic content", CreatedBy: "session"})
	loaded := managedTool[memory.GetResult](t, r.bound(legacy), "memory_get", map[string]string{"id": node.ID})
	found := managedTool[memory.SearchResult](t, r.bound(credential), "memory_search", memory.SearchInput{Query: "synthetic"})
	if loaded.Node.ID != node.ID || len(found.Nodes) != 1 {
		t.Fatal("memory read/write/search lost")
	}
	renamed := managedTool[broker.Session](t, r.bound(credential), "rename_session", broker.RenameRequest{RegistrationID: got.RegistrationID, ChannelID: "after-upgrade", Name: "renamed"})
	if renamed.Name != "renamed" {
		t.Fatal("rename lost")
	}
	renewed := managedTool[broker.RegisterResult](t, r.bound(credential), "renew_lease", broker.RenewRequest{RegistrationID: got.RegistrationID, ChannelID: "after-upgrade"})
	if renewed.Code != "" {
		t.Fatal("lease renewal lost")
	}
	// Existing parent still uses the human-approved flow; no auto admission.
	third := r.authorize(parent, protectedRequest(13, "ignored"), "third")
	if r.mcpStatus(parent) != 401 || r.mcpStatus(third) == 401 {
		t.Fatal("project/session credential boundary changed")
	}
	page := r.browser(http.MethodGet, "/projects/sessions?project=test", nil)
	if page.Code != 200 {
		t.Fatal("panel session listing lost")
	}
	stopped := managedTool[struct {
		OK bool `json:"ok"`
	}](t, r.bound(legacy), "stop_session", map[string]string{"name": "legacy"})
	if !stopped.OK {
		t.Fatal("legacy detach permission lost")
	}
	if r.close() != nil {
		t.Fatal("shutdown failed")
	}
	store, err = controlpanel.OpenSQLite(controlDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, p := range store.Projects() {
		for _, token := range p.Tokens {
			if token.ScopeVersion != access.ScopeVersion || !slices.Equal(token.Scopes, access.LegacyV1Scopes()) || access.HasScope(token.Scopes, access.SessionRegisterAuto) {
				t.Fatal("upgrade granted wrong scopes")
			}
		}
	}
}
