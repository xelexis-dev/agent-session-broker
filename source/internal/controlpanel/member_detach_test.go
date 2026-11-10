package controlpanel

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDeveloperDetachesOwnSessionAfterRevokingToken(t *testing.T) {
	s := userStore(t)
	raw, err := s.IssueTokenFor("developer", "demo", "dev-session", "developer")
	if err != nil {
		t.Fatal(err)
	}
	grant, _, ok := s.authorize("demo", raw)
	if !ok {
		t.Fatal("fixture grant denied")
	}
	if err := grant.Claim(`["codex","sid","pc"]`); err != nil {
		t.Fatal(err)
	}
	tokenID := s.ProjectsFor("developer")[0].Tokens[0].ID
	b := &ownedPanelBackend{}
	developer := loginPanelUser(t, New(s, b, http.NotFoundHandler()), "developer")
	if err := s.RevokeTokenFor("developer", "demo", tokenID); err != nil {
		t.Fatal(err)
	}
	if s.Authorize("demo", raw) {
		t.Fatal("revoked token still authorizes")
	}
	response := developer.call("POST", "/sessions/detach", url.Values{"project": {"demo"}, "id": {"owned"}})
	if response.Code != 303 {
		t.Fatalf("own revoked-token session detach = %d, want 303", response.Code)
	}
	if len(b.detached) != 1 || b.detached[0] != "demo/owned" {
		t.Fatal("authorized detach did not select the owned registration")
	}
}

// The real store and device approval path establish ownership. The backend only
// records the selected registration and suspension operation at the boundary.
type memberSessionBackend struct {
	reservedGrantBackend
	changes []string
}

func (b *memberSessionBackend) SetSuspended(_ context.Context, _, id string, suspended bool) error {
	b.changes = append(b.changes, id+":"+strconv.FormatBool(suspended))
	for i := range b.sessions {
		if b.sessions[i].ID == id {
			b.sessions[i].Suspended = suspended
		}
	}
	return nil
}

func memberSessionFixture(t *testing.T) (*deviceFixture, *memberSessionBackend, string, string) {
	t.Helper()
	f := newDeviceFixture(t)
	ownerRaw := f.raw
	var err error
	f.raw, err = f.store.IssueProjectTokenFor("developer", "demo", "own harness")
	if err != nil {
		t.Fatal(err)
	}
	own, req := f.approve(t, "owner", "11111111-1111-4111-8111-111111111111", "dev-session")
	parentID := ""
	for _, token := range f.store.ProjectsFor("owner")[0].Tokens {
		if token.Kind == tokenProject && token.OwnerID == "developer" {
			parentID = token.ID
		}
	}
	if parentID == "" {
		t.Fatal("developer project token missing")
	}
	f.raw = ownerRaw
	_, other := f.approve(t, "owner", "22222222-2222-4222-8222-222222222222", "owner-session")
	b := &memberSessionBackend{reservedGrantBackend: reservedGrantBackend{sessions: []Session{
		{ID: "owned", Name: "dev-session", Harness: req.Harness, SessionID: req.SessionID, ClientID: req.InstanceID, Status: "alive"},
		{ID: "other", Name: "owner-session", Harness: other.Harness, SessionID: other.SessionID, ClientID: other.InstanceID, Status: "alive"},
	}}}
	return f, b, parentID, own.AccessToken
}

func sessionRow(t *testing.T, body, name string) string {
	t.Helper()
	for _, row := range strings.Split(body, "</tr>") {
		if strings.Contains(row, "<strong>"+name+"</strong>") {
			return row
		}
	}
	t.Fatal("session row missing")
	return ""
}

func TestRevokedTokenSessionManagementUsesCurrentMemberRole(t *testing.T) {
	for _, tc := range []struct {
		name, actor, target, newRole string
		changeRole, allowed          bool
	}{
		{name: "developer own", actor: "developer", target: "owned", allowed: true},
		{name: "developer other", actor: "developer", target: "other"},
		{name: "owner own", actor: "owner", target: "owned", allowed: true},
		{name: "owner all", actor: "owner", target: "other", allowed: true},
		{name: "instance admin", actor: "admin", target: "owned", allowed: true},
		{name: "removed owner of grant", actor: "developer", target: "owned", changeRole: true},
		{name: "demoted owner of grant", actor: "developer", target: "owned", changeRole: true, newRole: "viewer"},
		{name: "viewer", actor: "viewer", target: "owned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, b, parentID, raw := memberSessionFixture(t)
			c := loginPanelUser(t, New(f.store, b, http.NotFoundHandler()), tc.actor)
			if err := f.store.RevokeTokenFor("developer", "demo", parentID); err != nil {
				t.Fatal(err)
			}
			if f.store.Authorize("demo", raw) {
				t.Fatal("revoked child credential still authorizes")
			}
			if tc.changeRole {
				if err := f.store.SetProjectMember("owner", "demo", "developer", tc.newRole); err != nil {
					t.Fatal(err)
				}
			}
			identity := `["claude","11111111-1111-4111-8111-111111111111","pc-example"]`
			if tc.target == "other" {
				identity = `["claude","22222222-2222-4222-8222-222222222222","pc-example"]`
			}
			if got := f.store.CanManageSession(tc.actor, "demo", identity); got != tc.allowed {
				t.Fatalf("store manage = %v, want %v", got, tc.allowed)
			}
			page := c.call("GET", "/projects/sessions?project=demo", nil)
			if tc.changeRole && tc.newRole == "" {
				if page.Code != 403 {
					t.Fatalf("removed member page = %d", page.Code)
				}
			} else {
				name := "dev-session"
				if tc.target == "other" {
					name = "owner-session"
				}
				row := sessionRow(t, page.Body.String(), name)
				for _, action := range []string{"detach", "suspend"} {
					if got := strings.Contains(row, `action="/sessions/`+action+`"`); got != tc.allowed {
						t.Fatalf("%s button=%v, want%v", action, got, tc.allowed)
					}
				}
				if tc.target == "owned" && !strings.Contains(row, `class="cell-account mono" data-label="계정">developer`) {
					t.Fatal("revocation hid registered account")
				}
			}
			want := 403
			if tc.allowed {
				want = 303
			}
			for _, action := range []string{"suspend", "unsuspend", "detach"} {
				w := c.call("POST", "/sessions/"+action, url.Values{"project": {"demo"}, "id": {tc.target}})
				if w.Code != want {
					t.Fatalf("%s = %d, want%d", action, w.Code, want)
				}
			}
			if tc.allowed {
				if len(b.detached) != 1 || b.detached[0] != tc.target || !reflect.DeepEqual(b.changes, []string{tc.target + ":true", tc.target + ":false"}) {
					t.Fatal("management reached wrong registration")
				}
			} else if len(b.detached) != 0 || len(b.changes) != 0 {
				t.Fatal("denied management reached backend")
			}
		})
	}
}

func TestReservedGrantReleaseAndButtonsUseCurrentMemberOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, actor, role   string
		changeRole, allowed bool
	}{
		{name: "developer own", actor: "developer", allowed: true},
		{name: "owner all", actor: "owner", allowed: true},
		{name: "instance admin", actor: "admin", allowed: true},
		{name: "other developer", actor: "outsider", role: "developer", changeRole: true},
		{name: "removed developer", actor: "developer", changeRole: true},
		{name: "demoted developer", actor: "developer", role: "viewer", changeRole: true},
		{name: "viewer", actor: "viewer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, b, _, raw := memberSessionFixture(t)
			b.sessions = nil
			id := reservedGrantID(t, f, "dev-session")
			c := loginPanelUser(t, New(f.store, b, http.NotFoundHandler()), tc.actor)
			if tc.changeRole {
				if err := f.store.SetProjectMember("owner", "demo", tc.actor, tc.role); err != nil {
					t.Fatal(err)
				}
			}
			if got := f.store.CanReleaseReservedGrant(tc.actor, "demo", id); got != tc.allowed {
				t.Fatalf("reserved permission=%v, want%v", got, tc.allowed)
			}
			page := c.call("GET", "/projects/sessions?project=demo", nil)
			if page.Code == 200 {
				hasButton := strings.Contains(page.Body.String(), `name="grant_id" value="`+id+`"`)
				if hasButton != tc.allowed {
					t.Fatalf("reserved release button=%v, want%v", hasButton, tc.allowed)
				}
			} else if page.Code != 403 {
				t.Fatalf("reserved page=%d", page.Code)
			}
			w := c.call("POST", "/sessions/detach", url.Values{"project": {"demo"}, "grant_id": {id}})
			want := 403
			if tc.allowed {
				want = 303
			}
			if w.Code != want {
				t.Fatalf("reserved release=%d, want%d", w.Code, want)
			}
			if len(b.detached) != 0 {
				t.Fatal("reserved release detached a registration")
			}
			if tc.allowed && f.store.Authorize("demo", raw) {
				t.Fatal("released grant still authenticates")
			}
		})
	}
}

func TestReapprovalUnderAnotherMemberDoesNotRestorePreviousOwnersAccess(t *testing.T) {
	f, b, parentID, _ := memberSessionFixture(t)
	if err := f.store.RevokeTokenFor("developer", "demo", parentID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetProjectMember("owner", "demo", "outsider", "developer"); err != nil {
		t.Fatal(err)
	}
	var err error
	f.raw, err = f.store.IssueProjectTokenFor("outsider", "demo", "replacement harness")
	if err != nil {
		t.Fatal(err)
	}
	f.approve(t, "outsider", "11111111-1111-4111-8111-111111111111", "dev-session")
	h := New(f.store, b, http.NotFoundHandler())
	for _, tc := range []struct {
		actor string
		want  int
	}{{"developer", 403}, {"outsider", 303}} {
		c := loginPanelUser(t, h, tc.actor)
		row := sessionRow(t, c.call("GET", "/projects/sessions?project=demo", nil).Body.String(), "dev-session")
		if got := strings.Contains(row, `action="/sessions/detach"`); got != (tc.want == 303) {
			t.Fatalf("%s reapproved session button=%v, want status%d", tc.actor, got, tc.want)
		}
		if w := c.call("POST", "/sessions/detach", url.Values{"project": {"demo"}, "id": {"owned"}}); w.Code != tc.want {
			t.Fatalf("%s reapproved session detach=%d, want%d", tc.actor, w.Code, tc.want)
		}
	}
}

func TestSweptGrantLeavesSessionManageableOnlyByOwnersAndAdmins(t *testing.T) {
	f, b, parentID, _ := memberSessionFixture(t)
	h := New(f.store, b, http.NotFoundHandler())
	developer := loginPanelUser(t, h, "developer")
	if err := f.store.RevokeTokenFor("developer", "demo", parentID); err != nil {
		t.Fatal(err)
	}
	identity := `["claude","11111111-1111-4111-8111-111111111111","pc-example"]`
	f.now = f.now.Add(30*24*time.Hour - time.Second)
	if err := f.store.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	if !f.store.CanManageSession("developer", "demo", identity) {
		t.Fatal("retained revoked grant lost ownership before retention elapsed")
	}
	f.now = f.now.Add(time.Second)
	if err := f.store.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	for _, token := range f.store.ProjectsFor("owner")[0].Tokens {
		if token.BoundIdentity == identity {
			t.Fatal("revoked grant was not swept at the 30-day boundary")
		}
	}
	if f.store.ProjectRole("developer", "demo") != "developer" {
		t.Fatal("fixture removed membership instead of sweeping history")
	}
	if f.store.CanManageSession("developer", "demo", identity) {
		t.Fatal("swept grant still confers member ownership")
	}
	row := sessionRow(t, developer.call("GET", "/projects/sessions?project=demo", nil).Body.String(), "dev-session")
	if strings.Contains(row, `action="/sessions/detach"`) {
		t.Fatal("swept grant still exposes member detach button")
	}
	if w := developer.call("POST", "/sessions/detach", url.Values{"project": {"demo"}, "id": {"owned"}}); w.Code != 403 {
		t.Fatalf("member after sweep=%d, want403", w.Code)
	}
	if len(b.detached) != 0 {
		t.Fatal("member without retained evidence reached backend")
	}
	for _, actor := range []string{"owner", "admin"} {
		c := loginPanelUser(t, h, actor)
		if !f.store.CanManageSession(actor, "demo", identity) {
			t.Fatalf("%s lost project-wide management after sweep", actor)
		}
		row := sessionRow(t, c.call("GET", "/projects/sessions?project=demo", nil).Body.String(), "dev-session")
		if !strings.Contains(row, `action="/sessions/detach"`) {
			t.Fatalf("%s detach button missing after sweep", actor)
		}
		if w := c.call("POST", "/sessions/detach", url.Values{"project": {"demo"}, "id": {"owned"}}); w.Code != 303 {
			t.Fatalf("%s after sweep=%d, want303", actor, w.Code)
		}
	}
	if !reflect.DeepEqual(b.detached, []string{"owned", "owned"}) {
		t.Fatal("owner/admin detach targeted wrong registration")
	}
}

func TestRecordedRegistrationOwnerSurvives31DayGrantSweep(t *testing.T) {
	f, b, parentID, _ := memberSessionFixture(t)
	// The registration snapshot is independent of policy history. Broker and
	// runtime tests separately verify authenticated owner persistence.
	b.sessions[0].OwnerID = "developer"
	if err := f.store.SetProjectMember("owner", "demo", "outsider", "developer"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RevokeTokenFor("developer", "demo", parentID); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(31 * 24 * time.Hour)
	if err := f.store.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	identity := sessionIdentity(b.sessions[0])
	for _, token := range f.store.ProjectsFor("owner")[0].Tokens {
		if token.BoundIdentity == identity {
			t.Fatal("grant was not swept")
		}
	}
	h := New(f.store, b, http.NotFoundHandler())
	for _, tc := range []struct {
		actor string
		want  int
	}{{"outsider", 403}, {"developer", 303}} {
		c := loginPanelUser(t, h, tc.actor)
		page := c.call("GET", "/projects/sessions?project=demo", nil)
		row := sessionRow(t, page.Body.String(), "dev-session")
		if strings.Contains(row, `action="/sessions/detach"`) != (tc.want == 303) {
			t.Fatal("fallback button disagrees with POST")
		}
		if !strings.Contains(row, `data-label="계정">developer`) {
			t.Fatal("fallback owner account missing")
		}
		w := c.call("POST", "/sessions/detach", url.Values{"project": {"demo"}, "id": {"owned"}})
		if w.Code != tc.want {
			t.Fatalf("%s detach after 31-day sweep=%d, want%d", tc.actor, w.Code, tc.want)
		}
	}
}

func TestRecordedOwnerFallbackHonorsCurrentRoleAndLatestGrant(t *testing.T) {
	for _, role := range []string{"developer", "viewer", ""} {
		t.Run("role:"+role, func(t *testing.T) {
			f, b, parentID, _ := memberSessionFixture(t)
			b.sessions[0].OwnerID = "developer"
			if err := f.store.RevokeTokenFor("developer", "demo", parentID); err != nil {
				t.Fatal(err)
			}
			f.now = f.now.Add(31 * 24 * time.Hour)
			if err := f.store.SweepExpired(); err != nil {
				t.Fatal(err)
			}
			if err := f.store.SetProjectMember("owner", "demo", "developer", role); err != nil {
				t.Fatal(err)
			}
			identity := sessionIdentity(b.sessions[0])
			want := role == "developer"
			if f.store.CanManageSession("developer", "demo", identity, b.sessions[0].OwnerID) != want {
				t.Fatal("fallback bypassed current role")
			}
			for _, actor := range []string{"owner", "admin"} {
				if !f.store.CanManageSession(actor, "demo", identity, b.sessions[0].OwnerID) {
					t.Fatal("owner/admin denied")
				}
			}
			if f.store.SessionOwners("demo", b.sessions...)[identity] != "developer" {
				t.Fatal("fallback account hidden")
			}
			rows, _, err := f.store.liveSessionRows("owner", "demo", b.sessions)
			if err != nil || len(rows) == 0 {
				t.Fatal("live snapshot failed", err)
			}
			if _, _, err := f.store.liveSessionRows("developer", "demo", b.sessions); role == "" && err == nil {
				t.Fatal("removed member live status allowed")
			}
			if role == "developer" {
				rows, _, err = f.store.liveSessionRows("developer", "demo", b.sessions)
				if err != nil || !rows[0].CanManage {
					t.Fatal("live fallback denied")
				}
			}
		})
	}
	f, b, _, _ := memberSessionFixture(t)
	b.sessions[0].OwnerID = "outsider"
	identity := sessionIdentity(b.sessions[0])
	if !f.store.CanManageSession("developer", "demo", identity, b.sessions[0].OwnerID) || f.store.CanManageSession("outsider", "demo", identity, b.sessions[0].OwnerID) {
		t.Fatal("recorded fallback overrode latest grant")
	}
	if f.store.SessionOwners("demo", b.sessions...)[identity] != "developer" {
		t.Fatal("display ignored latest grant")
	}
}
