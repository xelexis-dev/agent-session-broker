package serverruntime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func rebindGrant(t *testing.T, s *controlpanel.Store, raw string) access.Grant {
	t.Helper()
	var g access.Grant
	handler := controlpanel.New(s, nil, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { g, _ = access.FromContext(r.Context()) }))
	req := httptest.NewRequest("GET", "https://broker/mcp?project=project", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	if out.Code != 200 || g.Check == nil {
		t.Fatal("manual grant authentication failed")
	}
	return g
}
func issueManualRebindGrant(t *testing.T, s *controlpanel.Store, parent, session, name string) (access.Grant, string) {
	t.Helper()
	r := controlpanel.DeviceRequest{ClientID: "asb-client-v1", Harness: "claude", SessionID: session, InstanceID: "manual-client", RequestID: "approve-" + session}
	code, err := s.BeginDeviceAuthorization("project", parent, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApproveDeviceAuthorization("admin", code.UserCode, name, false); err != nil {
		t.Fatal(err)
	}
	issued, err := s.ExchangeDeviceToken("project", parent, code.DeviceCode, r.ClientID, false)
	if err != nil {
		t.Fatal(err)
	}
	return rebindGrant(t, s, issued.AccessToken), issued.AccessToken
}

type manualRebindFixture struct {
	root           string
	store          *controlpanel.Store
	remote         *broker.RemoteService
	backend        *protectedBackend
	source         broker.Session
	oldRaw, newRaw string
	grant          access.Grant
	request        protocol.RebindRequest
}

func newManualRebindFixture(t *testing.T) *manualRebindFixture {
	t.Helper()
	root := t.TempDir()
	s, err := controlpanel.OpenSQLite(filepath.Join(root, "control"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Setup("synthetic-manual-approval-password"); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProject("project", "token", true, ""); err != nil {
		t.Fatal(err)
	}
	if err = s.SetScopeIssuanceReady("admin", true); err != nil {
		t.Fatal(err)
	}
	parent, err := s.IssueProjectTokenFor("admin", "project", "manual-parent")
	if err != nil {
		t.Fatal(err)
	}
	oldID := "11111111-1111-4111-8111-111111111111"
	newID := "22222222-2222-4222-8222-222222222222"
	oldGrant, oldRaw := issueManualRebindGrant(t, s, parent, oldID, "original")
	remote, err := broker.NewSQLitePersistentRemoteService(filepath.Join(root, "registrations"))
	if err != nil {
		t.Fatal(err)
	}
	backend := newProtectedBackend("project", remote)
	r := broker.RegisterRequest{Holder: g4FixtureHolder(), Harness: "claude", SessionID: oldID, ClientID: "manual-client", PID: 1001, CWD: "/synthetic", Secret: "synthetic-secret-long-enough-for-registration", RequestID: "initial-register", ChannelID: "original-lease", ProcessKey: "same-process"}
	registered, err := backend.RegisterSession(access.WithGrant(t.Context(), oldGrant), r)
	if err != nil || registered.Code != "" {
		t.Fatal("source registration failed")
	}
	grant, newRaw := issueManualRebindGrant(t, s, parent, newID, "original-next")
	f := &manualRebindFixture{root: root, store: s, remote: remote, backend: backend, source: registered.Session, oldRaw: oldRaw, newRaw: newRaw, grant: grant, request: protocol.RebindRequest{Holder: g4FixtureHolder(), RegistrationID: registered.Session.RegistrationID, Harness: "claude", SessionID: newID, ClientID: r.ClientID, PID: r.PID, CWD: r.CWD, ProcessKey: r.ProcessKey, Secret: r.Secret, RequestID: "manual-rebind-request", ExpectedRevision: registered.Session.Revision}}
	t.Cleanup(func() { f.remote.Close(); f.store.Close() })
	return f
}
func (f *manualRebindFixture) checkApplied(t *testing.T) {
	t.Helper()
	result, ok := f.remote.InspectRebindResult(f.request.RegistrationID, f.request.RequestID, f.grant.TokenID, f.request.ExpectedRevision)
	if !ok || result.Name != "original" || result.Revision != f.source.Revision+1 {
		t.Fatal("rebind was not applied exactly once")
	}
	if f.store.Authorize("project", f.oldRaw) {
		t.Fatal("old credential not revoked")
	}
	g := rebindGrant(t, f.store, f.newRaw)
	if g.Nickname != "original" || g.Check(bindingIdentity("claude", f.request.SessionID, f.request.ClientID)) != nil {
		t.Fatal("caller credential name/binding not confirmed")
	}
	if len(f.store.PendingRebinds()) != 0 {
		t.Fatal("confirmed journal not removed")
	}
	audit, err := f.store.DirectAuditFor("admin", "project")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range audit {
		if e.Kind == "handover" && e.Handover != nil && e.Handover.RegistrationID == f.request.RegistrationID {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("handover audit count=%d", n)
	}
}

type manualRecoveryBackend struct{ backend mcpserver.Backend }

func (f manualRecoveryBackend) RemoteBackend(context.Context, string) (mcpserver.Backend, error) {
	return f.backend, nil
}
func TestManualRebindJournalFailureReplayAndRestart(t *testing.T) {
	for _, phase := range []string{"pending", "apply", "confirm"} {
		for _, restart := range []bool{false, true} {
			t.Run(phase+map[bool]string{false: "/replay", true: "/restart"}[restart], func(t *testing.T) {
				f := newManualRebindFixture(t)
				g := f.grant
				original := g.Rebind
				g.Rebind = func(step string, j protocol.RebindJournal, r protocol.RebindResult) error {
					if phase == "apply" && step == "confirm" {
						return controlpanel.ErrStorage
					}
					if err := original(step, j, r); err != nil {
						return err
					}
					if (phase == "pending" && step == "pending") || (phase == "confirm" && step == "confirm") {
						return controlpanel.ErrStorage
					}
					return nil
				}
				if _, err := f.backend.RebindSession(access.WithGrant(t.Context(), g), f.request); err == nil {
					t.Fatal("injected failure was lost")
				}
				if restart {
					if err := f.remote.Close(); err != nil {
						t.Fatal(err)
					}
					if err := f.store.Close(); err != nil {
						t.Fatal(err)
					}
					var err error
					f.store, err = controlpanel.OpenSQLite(filepath.Join(f.root, "control"))
					if err != nil {
						t.Fatal(err)
					}
					f.remote, err = broker.NewSQLitePersistentRemoteService(filepath.Join(f.root, "registrations"))
					if err != nil {
						t.Fatal(err)
					}
					f.backend = newProtectedBackend("project", f.remote)
					if err = recoverManualRebinds(f.store, manualRecoveryBackend{f.backend}); err != nil {
						t.Fatal(err)
					}
					if len(f.store.PendingRebinds()) != 0 {
						t.Fatal("startup did not resolve journal")
					}
					if phase == "pending" && !f.store.Authorize("project", f.oldRaw) {
						t.Fatal("unapplied startup journal changed original credential")
					}
				}
				fresh := rebindGrant(t, f.store, f.newRaw)
				result, err := f.backend.RebindSession(access.WithGrant(t.Context(), fresh), f.request)
				if err != nil || result.Name != "original" {
					t.Fatal("journal replay failed", err)
				}
				again, err := f.backend.RebindSession(access.WithGrant(t.Context(), rebindGrant(t, f.store, f.newRaw)), f.request)
				if err != nil || again.Revision != result.Revision {
					t.Fatal("idempotent replay applied a second handover")
				}
				f.checkApplied(t)
			})
		}
	}
}
func TestManualRebindProofAndAuthorizationGuards(t *testing.T) {
	for _, kind := range []string{"secret", "client", "owner", "suspended", "other-pid", "revision", "credential-identity", "scope", "static", "project", "harness"} {
		t.Run(kind, func(t *testing.T) {
			f := newManualRebindFixture(t)
			r, g := f.request, f.grant
			switch kind {
			case "secret":
				r.Secret = "wrong"
			case "client":
				r.ClientID = "other"
			case "owner":
				g.OwnerID = "another-owner"
			case "suspended":
				if err := f.remote.SetSuspended(t.Context(), f.source.RegistrationID, true); err != nil {
					t.Fatal(err)
				}
			case "other-pid":
				r.PID++
			case "revision":
				r.ExpectedRevision++
			case "credential-identity":
				r.SessionID = "33333333-3333-4333-8333-333333333333"
			case "scope":
				g.Scopes = []string{access.SessionRead}
			case "static":
				g.ParentID = ""
			case "project":
				g.Project = "another"
			case "harness":
				r.Harness = "codex"
			}
			if _, err := f.backend.RebindSession(access.WithGrant(t.Context(), g), r); err == nil {
				t.Fatal("unsafe rebind accepted")
			}
			if len(f.store.PendingRebinds()) != 0 {
				t.Fatal("definitive refusal left a journal")
			}
			if !f.store.Authorize("project", f.oldRaw) {
				t.Fatal("refusal revoked original credential")
			}
			sessions, err := f.remote.ListSessions(t.Context())
			if err != nil || len(sessions) != 1 || sessions[0].ID != f.source.ID {
				t.Fatal("refusal created or rebound a registration")
			}
		})
	}
}
