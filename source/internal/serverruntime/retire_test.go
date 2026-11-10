package serverruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
	"github.com/xelexis-dev/agent-session-broker/source/internal/memory"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"time"
)

func retireClient(t *testing.T, backend *protectedBackend, grant access.Grant) *mcp.ClientSession {
	t.Helper()
	mem, err := memory.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mem.Close() })
	server := mcpserver.New(backend, mem)
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			return next(access.WithGrant(ctx, grant), method, req)
		}
	})
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "retire-test", Version: "0.17.3"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestRetireOrphanReleasesStoppedName(t *testing.T) {
	store := protectedStore(t)
	remote := broker.NewRemoteService()
	defer remote.Close()
	b := newProtectedBackend("project", remote)
	manager, _ := manageRegistered(t, store, b, 181, "admin", "manager")
	old, a := manageRegistered(t, store, b, 182, "admin", "X")
	replacement, bs := manageRegistered(t, store, b, 183, "admin", "B")
	ctx := access.WithGrant(t.Context(), manager)
	if err := b.StopSession(ctx, a.RegistrationID); err != nil {
		t.Fatal(err)
	}
	if err := (panelBackend{store: store}).removeDetached(t.Context(), b, "project", a, false); err != nil {
		t.Fatal(err)
	}
	rename := broker.RenameRequest{RegistrationID: bs.RegistrationID, ChannelID: protectedRequest(183, "B").ChannelID, Name: "X"}
	var rejection protocol.RenameRejection
	if _, err := b.RenameSession(access.WithGrant(t.Context(), replacement), rename); !errors.As(err, &rejection) || rejection.Holder != "orphaned-credential" {
		t.Fatal("fixture must retain orphan reservation", err)
	}
	cs := retireClient(t, b, manager)
	result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "retire_session", Arguments: map[string]string{"name": "X"}})
	if err != nil || result.IsError {
		t.Fatalf("retire_session must release orphan reservation: error=%v result=%+v", err, result)
	}
	if _, err := b.RenameSession(access.WithGrant(t.Context(), replacement), rename); err != nil {
		t.Fatal("retired name remains reserved", err)
	}
	if err := old.Check(bindingIdentity(a.Harness, a.ID, a.ClientID)); err == nil {
		t.Fatal("retired credential still accepted")
	}
}

func TestRetireRefusalsLeaveTargetUnchanged(t *testing.T) {
	for _, scenario := range []string{"self", "suspended", "held-name", "both-selectors", "no-selector", "unused", "scope", "other-owner", "viewer", "ownerless", "caller-detached", "caller-suspended", "revoked", "demoted", "not-found", "cross-project"} {
		t.Run(scenario, func(t *testing.T) {
			s := protectedStore(t)
			manageUsers(t, s)
			remote := broker.NewRemoteService()
			defer remote.Close()
			b := newProtectedBackend("project", remote)
			manager, caller := manageRegistered(t, s, b, 191, "owner", "caller")
			_, target := manageRegistered(t, s, b, 192, "owner", "target")
			r := protocol.RetireRequest{RegistrationID: target.RegistrationID}
			want := "forbidden"
			switch scenario {
			case "self":
				r.RegistrationID = caller.RegistrationID
				want = "self-retire-denied"
			case "suspended":
				remote.SetSuspended(t.Context(), target.RegistrationID, true)
				want = "target-suspended"
			case "held-name":
				r = protocol.RetireRequest{Name: target.Name}
				want = "name-held-by-registration"
			case "both-selectors":
				r.Name = target.Name
				want = "invalid-selector"
			case "no-selector":
				r = protocol.RetireRequest{}
				want = "invalid-selector"
			case "unused":
				manageGrant(t, s, "owner", "unused")
				r = protocol.RetireRequest{Name: "unused"}
				want = "credential-unused"
			case "not-found":
				r = protocol.RetireRequest{Name: "never-used"}
				want = "not-found"
			case "cross-project":
				manager.Project = "foreign"
			case "scope":
				manager.Scopes = []string{access.SessionRead}
			case "other-owner":
				manager, _ = manageRegistered(t, s, b, 193, "other", "other")
			case "viewer":
				s.SetProjectMember("admin", "project", "owner", "viewer")
			case "ownerless":
				manager.OwnerID = ""
			case "caller-detached":
				b.StopSession(access.WithGrant(t.Context(), manager), caller.RegistrationID)
			case "caller-suspended":
				remote.SetSuspended(t.Context(), caller.RegistrationID, true)
			case "revoked":
				s.RevokeToken("project", manager.TokenID)
			case "demoted":
				s.SetProjectMember("admin", "project", "owner", "developer") // same owner remains allowed, so use a different owner.
				_, target = manageRegistered(t, s, b, 194, "other", "other-target")
				r.RegistrationID = target.RegistrationID
			}
			before, _ := remote.GetSession(t.Context(), target.RegistrationID)
			_, err := b.RetireSession(access.WithGrant(t.Context(), manager), r)
			if scenario == "scope" {
				if err == nil {
					t.Fatal("missing scope accepted")
				}
			} else {
				var rejection protocol.RetireRejection
				if !errors.As(err, &rejection) || rejection.Code != want {
					t.Fatal("unexpected refusal", err, want)
				}
			}
			after, err := remote.GetSession(t.Context(), target.RegistrationID)
			if err != nil || before.Revision != after.Revision || before.ErrorCode != after.ErrorCode || before.Suspended != after.Suspended {
				t.Fatal("refusal changed target", err)
			}
		})
	}
}

func TestRetireOrphanAndReregisterSerialize(t *testing.T) {
	for i := 0; i < 8; i++ {
		s := protectedStore(t)
		remote := broker.NewRemoteService()
		b := newProtectedBackend("project", remote)
		manager, _ := manageRegistered(t, s, b, 201, "admin", "manager")
		old, a := manageRegistered(t, s, b, 202, "admin", "X")
		if err := b.StopSession(access.WithGrant(t.Context(), manager), a.RegistrationID); err != nil {
			t.Fatal(err)
		}
		if err := (panelBackend{store: s}).removeDetached(t.Context(), b, "project", a, false); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		retired := make(chan error, 1)
		registered := make(chan broker.RegisterResult, 1)
		go func() {
			<-start
			_, err := b.RetireSession(access.WithGrant(t.Context(), manager), protocol.RetireRequest{Name: "X"})
			retired <- err
		}()
		go func() {
			<-start
			v, _ := b.RegisterSession(access.WithGrant(t.Context(), old), protectedRequest(202, "X"))
			registered <- v
		}()
		close(start)
		retireErr, reg := <-retired, <-registered
		if retireErr == nil {
			if reg.Code == "" {
				t.Fatal("revoked orphan re-registered")
			}
		} else {
			var rejection protocol.RetireRejection
			if !errors.As(retireErr, &rejection) || rejection.Code != "name-held-by-registration" || reg.Code != "" {
				t.Fatal("unexpected race outcome", retireErr, reg.Code)
			}
		}
		remote.Close()
	}
}

func TestRetireHTTPResponseAndUnrelatedLongPollPreserved(t *testing.T) {
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
	if !found["retire_session"] {
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
	managedTool[protocol.RetireResult](t, bound(tokens["manager"]), "retire_session", map[string]string{"registrationId": sessions["target"].RegistrationID})
	select {
	case name := <-finished:
		if name != "target" {
			t.Fatal("wrong request cancelled")
		}
	case <-ctx.Done():
		t.Fatal("retire did not revoke target request")
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

func TestRenameRejectionHolderAndReadDisclosure(t *testing.T) {
	for _, state := range []string{"live-session", "inactive-registration", "detached-registration", "orphaned-credential", "unused-credential"} {
		for _, read := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/read=%v", state, read), func(t *testing.T) {
				s := protectedStore(t)
				remote := broker.NewRemoteService()
				defer remote.Close()
				b := newProtectedBackend("project", remote)
				g, current := manageRegistered(t, s, b, 211, "admin", "current")
				var target broker.Session
				if state == "unused-credential" {
					manageGrant(t, s, "admin", "held")
				} else {
					_, target = manageRegistered(t, s, b, 212, "admin", "held")
				}
				if state == "inactive-registration" {
					if err := remote.ReportIngressFailure(t.Context(), target.RegistrationID, protectedRequest(212, "").ChannelID); err != nil {
						t.Fatal(err)
					}
				}
				if state == "detached-registration" || state == "orphaned-credential" {
					if err := b.StopSession(access.WithGrant(t.Context(), g), target.RegistrationID); err != nil {
						t.Fatal(err)
					}
				}
				if state == "orphaned-credential" {
					if err := (panelBackend{store: s}).removeDetached(t.Context(), b, "project", target, false); err != nil {
						t.Fatal(err)
					}
				}
				if !read {
					g.Scopes = []string{access.SessionRename}
				}
				result, err := retireClient(t, b, g).CallTool(t.Context(), &mcp.CallToolParams{Name: "rename_session", Arguments: broker.RenameRequest{Name: "held", RegistrationID: current.RegistrationID, ChannelID: protectedRequest(211, "").ChannelID}})
				if err != nil || !result.IsError {
					t.Fatal("rename rejection absent", err)
				}
				data, err := json.Marshal(result.StructuredContent)
				if err != nil {
					t.Fatal(err)
				}
				var rejection protocol.RenameRejection
				if err := json.Unmarshal(data, &rejection); err != nil {
					t.Fatal(err)
				}
				if rejection.Code != "nickname-in-use" || rejection.Holder != state {
					t.Fatal("wrong classification", string(data))
				}
				heldRegistration := state == "live-session" || state == "inactive-registration" || state == "detached-registration"
				if (rejection.HolderRegistrationID != "") != (read && heldRegistration) {
					t.Fatal("holder disclosure wrong", rejection)
				}
			})
		}
	}
}
