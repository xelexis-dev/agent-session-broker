package serverruntime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
)

func manageGrant(t *testing.T, s *controlpanel.Store, actor, name string) access.Grant {
	t.Helper()
	raw, err := s.IssueTokenFor(actor, "project", name, actor)
	if err != nil {
		t.Fatal(err)
	}
	var grant access.Grant
	h := controlpanel.New(s, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { grant, _ = access.FromContext(r.Context()) }))
	req := httptest.NewRequest("GET", "https://broker/mcp?project=project", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal("grant unavailable", w.Code)
	}
	return grant
}

func TestSessionManageOwnerDetachesOther(t *testing.T) {
	store := protectedStore(t)
	remote := broker.NewRemoteService()
	defer remote.Close()
	backend := newProtectedBackend("project", remote)
	caller := manageGrant(t, store, "admin", "manager")
	out, err := backend.RegisterSession(access.WithGrant(t.Context(), caller), protectedRequest(71, "manager"))
	if err != nil || out.Code != "" {
		t.Fatal("caller setup", err, out.Code)
	}
	target := protectedRegistered(t, remote, protectedRequest(72, "target"))
	if err := backend.StopSession(access.WithGrant(t.Context(), caller), target.RegistrationID); err != nil {
		t.Fatal("Owner cannot detach another session", err)
	}
	current, err := remote.GetSession(context.Background(), target.RegistrationID)
	if err != nil || current.ErrorCode != broker.CodeDetached {
		t.Fatal("target was not detached")
	}
}

func manageUsers(t *testing.T, s *controlpanel.Store) {
	t.Helper()
	for _, actor := range []string{"owner", "developer", "other"} {
		if err := s.CreateUser("admin", actor, "synthetic-long-password", "user"); err != nil {
			t.Fatal(err)
		}
		role := "developer"
		if actor == "owner" {
			role = "owner"
		}
		if err := s.SetProjectMember("admin", "project", actor, role); err != nil {
			t.Fatal(err)
		}
	}
}
func manageRegistered(t *testing.T, s *controlpanel.Store, b *protectedBackend, n int, owner, name string) (access.Grant, broker.Session) {
	t.Helper()
	g := manageGrant(t, s, owner, name)
	out, err := b.RegisterSession(access.WithGrant(t.Context(), g), protectedRequest(n, name))
	if err != nil || out.Code != "" {
		t.Fatal("managed registration setup", err, out.Code)
	}
	return g, out.Session
}
func manageOperation(b *protectedBackend, ctx context.Context, kind, target string) error {
	switch kind {
	case "detached":
		return b.StopSession(ctx, target)
	case "suspended":
		return b.SuspendSession(ctx, target)
	default:
		return b.UnsuspendSession(ctx, target)
	}
}
func TestSessionManageRoleMatrix(t *testing.T) {
	for _, tc := range []struct {
		actor, targetOwner string
		allowed            bool
	}{
		{"owner", "owner", true}, {"owner", "developer", true}, {"developer", "developer", true}, {"developer", "owner", false}, {"developer", "other", false},
	} {
		for _, kind := range []string{"detached", "suspended", "unsuspended"} {
			t.Run(tc.actor+"/"+tc.targetOwner+"/"+kind, func(t *testing.T) {
				s := protectedStore(t)
				manageUsers(t, s)
				remote := broker.NewRemoteService()
				defer remote.Close()
				b := newProtectedBackend("project", remote)
				g, _ := manageRegistered(t, s, b, 81, tc.actor, "caller")
				_, target := manageRegistered(t, s, b, 82, tc.targetOwner, "target")
				if kind == "unsuspended" {
					if err := remote.SetSuspended(t.Context(), target.RegistrationID, true); err != nil {
						t.Fatal(err)
					}
				}
				err := manageOperation(b, access.WithGrant(t.Context(), g), kind, target.RegistrationID)
				if (err == nil) != tc.allowed {
					t.Fatalf("allowed=%v error=%v", tc.allowed, err)
				}
				got, _ := remote.GetSession(t.Context(), target.RegistrationID)
				if tc.allowed && (got.LastManagement == nil || got.LastManagement.Kind != kind || got.LastManagement.ActorName != "caller") {
					t.Fatal("management state missing")
				}
			})
		}
	}
}
func TestSessionManageScopeSelfAndCallerState(t *testing.T) {
	for _, scenario := range []string{"missing-scope", "ownerless", "viewer", "self", "caller-suspended", "caller-detached", "cross-project"} {
		for _, kind := range []string{"detached", "suspended", "unsuspended"} {
			t.Run(scenario+"/"+kind, func(t *testing.T) {
				s := protectedStore(t)
				remote := broker.NewRemoteService()
				defer remote.Close()
				b := newProtectedBackend("project", remote)
				g, caller := manageRegistered(t, s, b, 91, "admin", "caller")
				target := protectedRegistered(t, remote, protectedRequest(92, "target"))
				selector := target.RegistrationID
				want := false
				switch scenario {
				case "missing-scope":
					g.Scopes = []string{access.SessionRead}
				case "ownerless":
					g.OwnerID = ""
				case "viewer":
					g.Manage = func(string, string) error { return controlpanel.ErrForbidden }
				case "self":
					selector = caller.RegistrationID
					want = kind == "detached"
				case "caller-suspended":
					if err := remote.SetSuspended(t.Context(), caller.RegistrationID, true); err != nil {
						t.Fatal(err)
					}
				case "caller-detached":
					if err := remote.StopSession(t.Context(), caller.RegistrationID); err != nil {
						t.Fatal(err)
					}
				case "cross-project":
					g.Project = "foreign"
				}
				err := manageOperation(b, access.WithGrant(t.Context(), g), kind, selector)
				if (err == nil) != want {
					t.Fatalf("allowed=%v error=%v", want, err)
				}
				// Ownerless/viewer-bound credentials retain self detach and cannot self suspend.
				if scenario == "ownerless" || scenario == "viewer" || scenario == "missing-scope" {
					err = manageOperation(b, access.WithGrant(t.Context(), g), kind, caller.RegistrationID)
					selfWant := kind == "detached" && scenario != "missing-scope"
					if (err == nil) != selfWant {
						t.Fatalf("self allowed=%v error=%v", selfWant, err)
					}
				}
			})
		}
	}
}
func TestSessionManageMutationRechecksLivePolicy(t *testing.T) {
	for _, change := range []string{"revoke", "demote", "remove"} {
		for _, kind := range []string{"detached", "suspended", "unsuspended"} {
			t.Run(change+"/"+kind, func(t *testing.T) {
				s := protectedStore(t)
				manageUsers(t, s)
				remote := broker.NewRemoteService()
				defer remote.Close()
				b := newProtectedBackend("project", remote)
				g, _ := manageRegistered(t, s, b, 101, "owner", "caller")
				_, target := manageRegistered(t, s, b, 102, "owner", "target")
				calls := 0
				check := g.Manage
				g.Manage = func(identity, owner string) error {
					err := check(identity, owner)
					calls++
					if calls == 1 && err == nil {
						switch change {
						case "revoke":
							err = s.RevokeToken("project", g.TokenID)
						case "demote":
							err = s.SetProjectMember("admin", "project", "owner", "viewer")
						case "remove":
							err = s.SetProjectMember("admin", "project", "owner", "")
						}
						if err != nil {
							t.Fatal(err)
						}
					}
					return err
				}
				if err := manageOperation(b, access.WithGrant(t.Context(), g), kind, target.RegistrationID); err == nil {
					t.Fatal("policy change after precheck allowed mutation")
				}
				got, _ := remote.GetSession(t.Context(), target.RegistrationID)
				if got.ErrorCode != target.ErrorCode || got.Revision != target.Revision || got.LastManagement != nil {
					t.Fatal("denied mutation changed target")
				}
			})
		}
	}
}

func TestSessionManageSameAutoParentAndAudit(t *testing.T) {
	s := protectedStore(t)
	if err := s.SetScopeIssuanceReady("admin", true); err != nil {
		t.Fatal(err)
	}
	parent, err := s.IssueProjectTokenWithOptionsFor("admin", "project", "target-parent", controlpanel.ProjectTokenOptions{Scopes: append(access.LegacyV1Scopes(), access.SessionRegisterAuto)})
	if err != nil {
		t.Fatal(err)
	}
	remote := broker.NewRemoteService()
	defer remote.Close()
	b := newProtectedBackend("project", remote)
	grants := []access.Grant{}
	sessions := []broker.Session{}
	for _, n := range []int{131, 132} {
		r := protectedRequest(n, "ignored")
		exchange, err := s.ExchangeDirect("project", parent, controlpanel.DirectRequest{DeviceRequest: controlpanel.DeviceRequest{ClientID: "asb-client-v1", Harness: r.Harness, SessionID: r.SessionID, InstanceID: r.ClientID, RequestID: r.RequestID}}, controlpanel.DirectProof{Absent: true}, "synthetic", false)
		if err != nil {
			t.Fatal("auto exchange", err)
		}
		var g access.Grant
		h := controlpanel.New(s, nil, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { g, _ = access.FromContext(r.Context()) }))
		req := httptest.NewRequest("GET", "https://broker/mcp?project=project", nil)
		req.Header.Set("Authorization", "Bearer "+exchange.AccessToken)
		h.ServeHTTP(httptest.NewRecorder(), req)
		result, err := b.RegisterSession(access.WithGrant(t.Context(), g), r)
		if err != nil || result.Code != "" {
			t.Fatal("auto registration", err, result.Code)
		}
		grants = append(grants, g)
		sessions = append(sessions, result.Session)
	}
	if grants[0].ParentID != grants[1].ParentID {
		t.Fatal("fixture parent differs")
	}
	ctx := access.WithGrant(t.Context(), grants[0])
	for _, kind := range []string{"suspended", "unsuspended", "detached"} {
		if err := manageOperation(b, ctx, kind, sessions[1].RegistrationID); err != nil {
			t.Fatal(kind, err)
		}
	}
	events, err := s.DirectAuditFor("admin", "project")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range events {
		if e.Management != nil {
			count++
			m := e.Management
			if m.ActorName != sessions[0].Name || m.ActorRegistrationID != sessions[0].RegistrationID || m.TargetName != sessions[1].Name || m.TargetRegistrationID != sessions[1].RegistrationID || m.TargetParentLabel != "target-parent" || e.Binding != "" || e.ParentID != "" || e.GrantID != "" || m.At.IsZero() {
				t.Fatal("unsafe or incomplete management event")
			}
			data, _ := json.Marshal(e)
			if strings.Contains(string(data), bindingIdentity("codex", protectedRequest(132, "").SessionID, protectedRequest(132, "").ClientID)) || strings.Contains(string(data), parent) {
				t.Fatal("management audit exposed identity or credential")
			}
		}
	}
	if count != 3 {
		t.Fatal("management audit count", count)
	}
}

func TestSessionManagePanelAuditAndPersistence(t *testing.T) {
	s := protectedStore(t)
	if err := s.SetScopeIssuanceReady("admin", true); err != nil {
		t.Fatal(err)
	}
	registry, err := groups.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	factory := mcpserver.NewManagedProjectFactory(registry, persistentBackend)
	defer factory.Close()
	adapter := panelBackend{factory: factory, store: s}
	service, err := factory.RemoteBackend(t.Context(), "project")
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.(broker.DurableRegistrar).RegisterSession(t.Context(), protectedRequest(141, "panel-target"))
	if err != nil || result.Code != "" {
		t.Fatal("setup")
	}
	id := result.Session.RegistrationID
	for _, paused := range []bool{true, false} {
		if err := adapter.SetSuspended(t.Context(), "project", id, paused); err != nil {
			t.Fatal(err)
		}
	}
	current, _ := service.GetSession(t.Context(), id)
	if current.LastManagement == nil || current.LastManagement.Kind != "unsuspended" || current.LastManagement.ActorName != "panel" {
		t.Fatal("panel last management missing")
	}
	if err := adapter.Detach(t.Context(), "project", id); err != nil {
		t.Fatal(err)
	}
	events, err := s.DirectAuditFor("admin", "project")
	if err != nil || len(events) != 3 {
		t.Fatal("panel audit", err, len(events))
	}
	for _, e := range events {
		if e.Management == nil || e.Management.ActorName != "panel" || e.Management.TargetName != "panel-target" || e.Management.TargetRegistrationID != id {
			t.Fatal("panel target audit lost")
		}
	}
}

type manageBlockingBackend struct {
	*protectedBackend
	started  chan string
	finished chan string
}

func (b *manageBlockingBackend) GetSession(ctx context.Context, name string) (broker.Session, error) {
	// Panel authorization also reads sessions.
	if name != "synthetic" {
		return b.protectedBackend.GetSession(ctx, name)
	}
	g, _ := access.FromContext(ctx)
	b.started <- g.Nickname
	<-ctx.Done()
	b.finished <- g.Nickname
	return broker.Session{}, ctx.Err()
}
func TestSessionManageMCPSuspendCancelsMatchingInflightRequests(t *testing.T) {
	dir := t.TempDir()
	s, err := controlpanel.OpenSQLite(filepath.Join(dir, "control"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Setup("synthetic-long-password"); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateProject("test"); err != nil {
		t.Fatal(err)
	}
	tokens := map[string]string{}
	for _, name := range []string{"manager", "target", "other"} {
		tokens[name], err = s.IssueToken("test", name)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	started, finished := make(chan string, 2), make(chan string, 2)
	h, closeHandler, err := newManagedHandler(dir, func(groups.ProjectDirectory) (mcpserver.Backend, error) {
		return &manageBlockingBackend{protectedBackend: newProtectedBackend("test", broker.NewRemoteService()), started: started, finished: finished}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandler()
	bound := func(token string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
			h.ServeHTTP(w, r)
		})
	}
	sessions := map[string]broker.Session{}
	for i, name := range []string{"manager", "target", "other"} {
		result := managedTool[broker.RegisterResult](t, bound(tokens[name]), "register_session", protectedRequest(151+i, name))
		if result.Code != "" {
			t.Fatal(result.Code)
		}
		sessions[name] = result.Session
	}
	var listed struct{ Tools []struct{ Name string } }
	managedRPC(t, bound(tokens["manager"]), "tools/list", map[string]any{}, &listed)
	found := map[string]bool{}
	for _, tool := range listed.Tools {
		found[tool.Name] = true
	}
	if !found["suspend_session"] || !found["unsuspend_session"] {
		t.Fatal("management tools missing")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	joined := make(chan struct{}, 2)
	for _, name := range []string{"target", "other"} {
		go func(name string) {
			defer func() { joined <- struct{}{} }()
			data := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"synthetic-test","version":"0.17.3"},"io.modelcontextprotocol/clientCapabilities":{}},"name":"get_session","arguments":{"name":"synthetic"}}}`
			req := httptest.NewRequest("POST", "https://broker/mcp?project=test", strings.NewReader(data)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("MCP-Protocol-Version", "2026-07-28")
			req.Header.Set("Mcp-Method", "tools/call")
			req.Header.Set("Mcp-Name", "get_session")
			req.Header.Set("Accept", "application/json, text/event-stream")
			bound(tokens[name]).ServeHTTP(httptest.NewRecorder(), req)
		}(name)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("inflight requests not started")
		}
	}
	managedTool[map[string]bool](t, bound(tokens["manager"]), "suspend_session", map[string]string{"name": sessions["target"].RegistrationID})
	select {
	case name := <-finished:
		if name != "target" {
			t.Fatal("wrong request cancelled")
		}
	case <-ctx.Done():
		t.Fatal("MCP suspend did not cancel target request")
	}
	select {
	case <-finished:
		t.Fatal("unrelated request cancelled")
	default:
	}
	cancel()
	for i := 0; i < 2; i++ {
		select {
		case <-joined:
		case <-time.After(time.Second):
			t.Fatal("request not joined")
		}
	}
}

func TestSessionManageCrossProjectSelectorIsProtected(t *testing.T) {
	s := protectedStore(t)
	remote := broker.NewRemoteService()
	defer remote.Close()
	b := newProtectedBackend("project", remote)
	g, _ := manageRegistered(t, s, b, 171, "admin", "caller")
	foreign := broker.NewRemoteService()
	defer foreign.Close()
	target := protectedRegistered(t, foreign, protectedRequest(172, "foreign"))
	for _, kind := range []string{"detached", "suspended", "unsuspended"} {
		if err := manageOperation(b, access.WithGrant(t.Context(), g), kind, target.RegistrationID); !errors.Is(err, errProtectedAccess) {
			t.Fatal("cross-project selector was not protected", err)
		}
	}
}
