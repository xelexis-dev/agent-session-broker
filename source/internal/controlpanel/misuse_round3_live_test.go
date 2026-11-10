package controlpanel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// Forward panel operations to a real in-process broker. The projection and
// detach cleanup follow serverruntime.panelBackend; runtime audit persistence
// and actual client diagnostics remain covered by runtime tests / PM's P5.
type round3LiveBackend struct {
	remote *broker.RemoteService
	store  *Store
}

func (b round3LiveBackend) Sessions(ctx context.Context, _ string) ([]Session, error) {
	rows, err := b.remote.ListSessions(ctx)
	owners := b.remote.RegistrationOwners()
	var out []Session
	for _, r := range rows {
		out = append(out, Session{ID: r.RegistrationID, RegistrationID: r.RegistrationID, SessionID: r.ID, ClientID: r.ClientID, Harness: r.Harness, Name: r.Name, OwnerID: owners[r.RegistrationID], Status: string(r.Status), Ready: r.AutomaticDeliveryReady, Suspended: r.Suspended, ErrorCode: r.ErrorCode})
	}
	return out, err
}
func (b round3LiveBackend) Detach(ctx context.Context, project, id string) error {
	selector := broker.SelectorRegistration + id
	if err := b.remote.StopSession(ctx, selector); err != nil {
		return err
	}
	var identity string
	if err := b.remote.RemoveDetachedSession(ctx, selector, func(r broker.Session) error {
		identity = DeviceRequest{Harness: r.Harness, SessionID: r.ID, InstanceID: r.ClientID}.identity()
		return b.store.RevokeSessionTokens(project, r.Name, identity)
	}); err != nil {
		return err
	}
	return b.store.ReleaseDirectRegistration(project, identity)
}
func (b round3LiveBackend) SetSuspended(ctx context.Context, _ string, id string, paused bool) error {
	return b.remote.SetSuspended(ctx, broker.SelectorRegistration+id, paused)
}
func round3LiveFixture(t *testing.T, s *Store, actor string) (round3LiveBackend, broker.RegisterRequest, broker.RegisterResult, string, access.Grant) {
	t.Helper()
	var raw string
	var err error
	if actor == "owner" {
		if err = s.SetScopeIssuanceReady("admin", true); err != nil {
			t.Fatal(err)
		}
		parent, issueErr := s.IssueProjectTokenWithOptionsFor(actor, "demo", "auto", ProjectTokenOptions{Scopes: append(access.LegacyV1Scopes(), access.SessionRegisterAuto)})
		if issueErr != nil {
			t.Fatal(issueErr)
		}
		child, exchangeErr := s.ExchangeDirect("demo", parent, directRequest(directForm()), DirectProof{}, "peer", false)
		if exchangeErr != nil {
			t.Fatal(exchangeErr)
		}
		raw = child.AccessToken
	} else {
		raw, err = s.IssueTokenFor(actor, "demo", "worker", actor)
		if err != nil {
			t.Fatal(err)
		}
	}
	g := directGrant(t, s, raw)
	remote := broker.NewRemoteService()
	t.Cleanup(func() { remote.Close() })
	remote.SetAdmission(directAdmissionFixture{})
	req := directRegisterRequest()
	registered, err := remote.RegisterSession(access.WithGrant(t.Context(), g), req)
	if err != nil || registered.Code != "" {
		t.Fatalf("fixture registration: code=%s error=%v", registered.Code, err)
	}
	if actor == "owner" {
		if err := confirmDirect(t, g, registered); err != nil {
			t.Fatal(err)
		}
	}
	return round3LiveBackend{remote, s}, req, registered, raw, g
}
func round3Resume(req broker.RegisterRequest, out broker.RegisterResult, challenger bool) broker.ResumeRequest {
	holder := *req.Holder
	lease := "holder-reconnect"
	if challenger {
		holder.PID++
		holder.Incarnation = strings.Repeat("d", 64)
		lease = "challenger-reconnect"
	}
	return broker.ResumeRequest{Holder: &holder, RegistrationID: out.Session.RegistrationID, Secret: req.Secret, Harness: req.Harness, SessionID: req.SessionID, ClientID: req.ClientID, PID: holder.PID, CWD: req.CWD, ChannelID: lease, RequestID: lease}
}
func TestRound3J1J5PanelDetachAndSuspendLiveHolder(t *testing.T) {
	for _, pause := range []bool{false, true} {
		name := "detach-live"
		if pause {
			name = "suspend-waiting-then-detach"
		}
		t.Run(name, func(t *testing.T) {
			s := userStore(t)
			b, req, registered, raw, _ := round3LiveFixture(t, s, "developer")
			h := New(s, b, http.NotFoundHandler())
			admin := loginPanelUser(t, h, "admin")
			challenger := round3Resume(req, registered, true)
			blocked, err := b.remote.ResumeSession(t.Context(), challenger)
			if err != nil || blocked.Code != protocol.CodeRegistrationInUse {
				t.Fatalf("challenger code=%s error=%v", blocked.Code, err)
			}
			rows, err := b.Sessions(t.Context(), "demo")
			if err != nil || len(rows) != 1 || rows[0].ID != registered.Session.RegistrationID || rows[0].DisplayState() != "ready" {
				t.Fatal("waiting challenger changed holder's single ready row")
			}
			page := admin.call("GET", "/projects/sessions?project=demo", nil)
			if page.Code != 200 || !strings.Contains(page.Body.String(), `action="/sessions/suspend"`) || strings.Contains(page.Body.String(), `action="/sessions/unsuspend"`) {
				t.Fatal("live holder controls incorrect")
			}
			values := url.Values{"project": {"demo"}, "id": {registered.Session.RegistrationID}}
			if pause {
				if w := admin.call("POST", "/sessions/suspend", values); w.Code != 303 {
					t.Fatalf("suspend=%d", w.Code)
				}
				for _, other := range []bool{false, true} {
					out, err := b.remote.ResumeSession(t.Context(), round3Resume(req, registered, other))
					if err != nil || out.Code != broker.CodeSuspended {
						t.Fatalf("suspended resume code=%s error=%v", out.Code, err)
					}
				}
				renewed, err := b.remote.RenewLease(t.Context(), broker.RenewRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: req.ChannelID})
				if err != nil || renewed.Code != broker.CodeSuspended {
					t.Fatal("old receiver renewed while suspended")
				}
				rows, err = b.Sessions(t.Context(), "demo")
				if err != nil || len(rows) != 1 || rows[0].DisplayState() != "suspended" {
					t.Fatal("suspended holder projection inconsistent")
				}
				page = admin.call("GET", "/projects/sessions?project=demo", nil)
				if page.Code != 200 || !strings.Contains(page.Body.String(), `action="/sessions/unsuspend"`) || strings.Contains(page.Body.String(), `action="/sessions/suspend"`) {
					t.Fatal("suspended controls incorrect")
				}
			}
			// A stale detach form must respect the same suspended guard as the UI.
			if pause {
				if w := admin.call("POST", "/sessions/detach", values); w.Code != 409 {
					t.Fatalf("suspended stale detach=%d", w.Code)
				}
				rows, err = b.Sessions(t.Context(), "demo")
				if err != nil || len(rows) != 1 || !rows[0].Suspended {
					t.Fatal("rejected detach changed suspended row")
				}
				if w := admin.call("POST", "/sessions/unsuspend", values); w.Code != 303 {
					t.Fatalf("unsuspend=%d", w.Code)
				}
			}
			if w := admin.call("POST", "/sessions/detach", values); w.Code != 303 {
				t.Fatalf("detach=%d", w.Code)
			}
			for _, other := range []bool{false, true} {
				out, err := b.remote.ResumeSession(t.Context(), round3Resume(req, registered, other))
				if err != nil || out.Code != broker.CodeNotFound {
					t.Fatalf("removed proof resume code=%s error=%v", out.Code, err)
				}
			}
			if rows, err = b.Sessions(t.Context(), "demo"); err != nil || len(rows) != 0 {
				t.Fatal("resume recreated or retained detached registration")
			}
			if _, _, ok := s.authorize("demo", raw); ok {
				t.Fatal("detached credential remains valid")
			}
		})
	}
}

func TestRound3J2MembershipChangeDuringLiveRequestAndOpenPanel(t *testing.T) {
	for _, tc := range []struct {
		name, actor, role, action string
		revoke                    bool
	}{
		{"remove", "developer", "", "remove", true},
		{"viewer", "developer", "viewer", "update", true},
		{"owner-to-developer", "owner", "developer", "update", true},
		{"promote", "developer", "owner", "update", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := userStore(t)
			// Keep another explicit owner so a downgrade is a valid membership change.
			if err := s.SetProjectMember("admin", "demo", "outsider", "owner"); err != nil {
				t.Fatal(err)
			}
			b, req, registered, raw, grant := round3LiveFixture(t, s, tc.actor)
			started, finished := make(chan struct{}), make(chan struct{})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			h := New(s, b, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
			admin := loginPanelUser(t, h, "admin")
			member := loginPanelUser(t, h, tc.actor)
			if w := member.call("GET", "/projects/settings?project=demo", nil); w.Code != 200 {
				t.Fatal("member form did not open")
			}
			request := httptest.NewRequest("POST", "https://broker/mcp?project=demo", nil).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer "+raw)
			go func() { defer close(finished); h.ServeHTTP(httptest.NewRecorder(), request) }()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("MCP request did not enter")
			}
			w := admin.call("POST", "/projects/members", url.Values{"project": {"demo"}, "username": {tc.actor}, "role": {tc.role}, "action": {tc.action}})
			if w.Code != 303 {
				t.Fatalf("membership change=%d", w.Code)
			}
			select {
			case <-finished:
			case <-ctx.Done():
				t.Fatal("permission change did not cancel old MCP request")
			}
			if grant.Check(bindingForDirect(req)) == nil {
				t.Fatal("old grant survived policy change")
			}
			_, _, authorized := s.authorize("demo", raw)
			if authorized == tc.revoke {
				t.Fatal("credential revocation differs from role downgrade policy")
			}
			// The old login cookie is still valid; each submitted form uses today's role.
			before := misuseSnapshot(t, s)
			if tc.role != "owner" {
				for _, route := range []string{"/projects/save", "/projects/members"} {
					w = member.call("POST", route, url.Values{"name": {"demo"}, "project": {"demo"}, "enabled": {"true"}, "username": {"viewer"}, "role": {"owner"}, "action": {"update"}})
					if w.Code != 403 {
						t.Fatalf("stale-role %s=%d", route, w.Code)
					}
				}
				if misuseSnapshot(t, s) != before {
					t.Fatal("stale role modified settings/members")
				}
			}
			want := 303
			if tc.role == "" || tc.role == "viewer" {
				want = 403
			}
			w = member.call("POST", "/sessions/suspend", url.Values{"project": {"demo"}, "id": {registered.Session.RegistrationID}})
			if w.Code != want {
				t.Fatalf("current-role own-session control=%d want=%d", w.Code, want)
			}
			if !tc.revoke {
				fresh := directGrant(t, s, raw)
				if fresh.Check(bindingForDirect(req)) != nil {
					t.Fatal("promoted member cannot reauthorize retained token")
				}
			}
		})
	}
}
