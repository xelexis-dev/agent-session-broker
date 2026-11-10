package serverruntime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
)

func ownerRuntimeGrant(t *testing.T, store *controlpanel.Store, owner string) access.Grant {
	t.Helper()
	raw, err := store.IssueTokenFor(owner, "project", "owned", owner)
	if err != nil {
		t.Fatal(err)
	}
	var grant access.Grant
	h := controlpanel.New(store, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { grant, _ = access.FromContext(r.Context()) }))
	req := httptest.NewRequest("GET", "https://broker/mcp?project=project", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || grant.OwnerID != owner {
		t.Fatal("authenticated owner missing", w.Code)
	}
	return grant
}

func TestRegistrationOwnerUsesAuthenticatedGrantAndResumeTransfers(t *testing.T) {
	store := protectedStore(t)
	for _, user := range []string{"alice", "bob"} {
		if err := store.CreateUser("admin", user, "fixture-password-long-enough", "user"); err != nil {
			t.Fatal(err)
		}
		if err := store.SetProjectMember("admin", "project", user, "developer"); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	remote, err := broker.NewSQLitePersistentRemoteService(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { remote.Close() }()
	backend := newProtectedBackend("project", remote)
	grant := ownerRuntimeGrant(t, store, "alice")
	req := protectedRequest(1, "owned")
	result, err := backend.RegisterSession(access.WithGrant(t.Context(), grant), req)
	if err != nil || result.Code != "" {
		t.Fatal("register failed", err, result.Code)
	}
	id := result.Session.RegistrationID
	if remote.RegistrationOwners()[id] != "alice" {
		t.Fatal("wrong authenticated owner")
	}
	wire, _ := json.Marshal(result)
	if strings.Contains(string(wire), "owner_id") || strings.Contains(string(wire), "alice") {
		t.Fatal("owner leaked into protocol")
	}
	// Revocation permits a new owner's approval while the registration stays.
	for _, token := range store.Projects()[0].Tokens {
		if token.OwnerID == "alice" {
			if err := store.RevokeTokenFor("admin", "project", token.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	next := ownerRuntimeGrant(t, store, "bob")
	if err := next.Claim(bindingIdentity(req.Harness, req.SessionID, req.ClientID)); err != nil {
		t.Fatal(err)
	}
	resumed, err := backend.ResumeSession(access.WithGrant(t.Context(), next), broker.ResumeRequest{Holder: g4FixtureHolder(), RegistrationID: id, Harness: req.Harness, SessionID: req.SessionID, ClientID: req.ClientID, PID: req.PID, CWD: req.CWD, Secret: req.Secret, ChannelID: "lease-new-owner", RequestID: "resume-owner", ExpectedRevision: result.Session.Revision})
	if err != nil || resumed.Code != "" {
		t.Fatal("resume failed", err, resumed.Code)
	}
	if remote.RegistrationOwners()[id] != "bob" {
		t.Fatal("resume reapproval retained previous owner")
	}
	if err := remote.Close(); err != nil {
		t.Fatal(err)
	}
	remote, err = broker.NewSQLitePersistentRemoteService(root)
	if err != nil {
		t.Fatal(err)
	}
	if remote.RegistrationOwners()[id] != "bob" {
		t.Fatal("restart lost transferred owner")
	}
}

func TestManagedRegistrationOwnerBackfillsBeforeRetentionAndReachesPanel(t *testing.T) {
	root := t.TempDir()
	store, err := controlpanel.OpenSQLite(filepath.Join(root, "control"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Setup("fixture-password-long-enough"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProject("project", "token", true, ""); err != nil {
		t.Fatal(err)
	}
	grant := ownerRuntimeGrant(t, store, "admin")
	req := protectedRequest(1, "owned")
	if err := grant.Claim(bindingIdentity(req.Harness, req.SessionID, req.ClientID)); err != nil {
		t.Fatal(err)
	}
	registry, err := groups.NewRegistry(filepath.Join(root, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	factory := mcpserver.NewManagedProjectFactory(registry, persistentBackend)
	service, err := factory.RemoteBackend(t.Context(), "project")
	if err != nil {
		t.Fatal(err)
	}
	registered, err := service.(broker.DurableRegistrar).RegisterSession(context.Background(), req)
	if err != nil || registered.Code != "" {
		t.Fatal("legacy register failed", err)
	}
	id := registered.Session.RegistrationID
	if err := factory.Close(); err != nil {
		t.Fatal(err)
	}
	registry.Close()
	store.Close()
	_, closeHandler, err := newManagedHandler(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := closeHandler(); err != nil {
		t.Fatal(err)
	}
	registry, err = groups.NewRegistry(filepath.Join(root, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	factory = mcpserver.NewManagedProjectFactory(registry, persistentBackend)
	defer factory.Close()
	rows, err := (panelBackend{factory: factory, registry: registry}).LiveSessions(t.Context(), "project")
	if err != nil || len(rows) != 1 || rows[0].OwnerID != "admin" || rows[0].RegistrationID != id {
		t.Fatal("startup backfill or panel owner lost", err)
	}
}
