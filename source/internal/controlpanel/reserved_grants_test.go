package controlpanel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type reservedGrantBackend struct {
	sessions []Session
	detached []string
	err      error
}

func (b *reservedGrantBackend) Sessions(context.Context, string) ([]Session, error) {
	return b.sessions, b.err
}
func (b *reservedGrantBackend) Detach(_ context.Context, _, id string) error {
	b.detached = append(b.detached, id)
	return nil
}

func reservedGrantID(t *testing.T, f *deviceFixture, name string) string {
	t.Helper()
	for _, p := range f.store.ProjectsFor("admin") {
		for _, token := range p.Tokens {
			if token.Nickname == name && !token.Revoked {
				return token.ID
			}
		}
	}
	t.Fatal("grant not found")
	return ""
}

func TestReservedGrantListedAndDetachReleasesOnlySelectedNickname(t *testing.T) {
	f := newDeviceFixture(t)
	var err error
	f.raw, err = f.store.IssueProjectTokenFor("developer", "demo", "developer harness")
	if err != nil {
		t.Fatal(err)
	}
	first, _ := f.approve(t, "developer", "11111111-1111-4111-8111-111111111111", "reserved-a")
	second, _ := f.approve(t, "owner", "22222222-2222-4222-8222-222222222222", "reserved-b")
	id := reservedGrantID(t, f, "reserved-a")
	b := &reservedGrantBackend{}
	c := loginPanelUser(t, New(f.store, b, http.NotFoundHandler()), "developer")
	body := c.call("GET", "/projects/sessions?project=demo", nil).Body.String()
	for _, want := range []string{"reserved-a", "reserved-b", en("session.connection.unregistered"), `name="grant_id" value="` + id + `"`, `data-confirm-action="session-grant-release"`} {
		if !strings.Contains(body, want) {
			t.Errorf("list lacks %s", want)
		}
	}
	if strings.Contains(body, first.AccessToken) || strings.Contains(body, second.AccessToken) {
		t.Fatal("credential leaked")
	}
	if w := c.call("POST", "/sessions/detach", url.Values{"project": {"demo"}, "grant_id": {id}}); w.Code != 303 {
		t.Fatalf("detach=%d", w.Code)
	}
	if len(b.detached) != 0 {
		t.Fatal("grant release detached a registration")
	}
	if f.store.Authorize("demo", first.AccessToken) || !f.store.Authorize("demo", second.AccessToken) {
		t.Fatal("release touched wrong grant")
	}
	body = c.call("GET", "/projects/sessions?project=demo", nil).Body.String()
	if strings.Contains(body, "reserved-a") {
		t.Fatal("released row remains")
	}
	f.approve(t, "developer", "33333333-3333-4333-8333-333333333333", "reserved-a")
}

func TestReservedGrantDetachPermissionsAndTargetValidation(t *testing.T) {
	for _, actor := range []string{"viewer", "outsider", "developer", "owner", "admin"} {
		t.Run(actor, func(t *testing.T) {
			f := newDeviceFixture(t)
			credential, _ := f.approve(t, "owner", "11111111-1111-4111-8111-111111111111", "reserved")
			id := reservedGrantID(t, f, "reserved")
			b := &reservedGrantBackend{}
			c := loginPanelUser(t, New(f.store, b, http.NotFoundHandler()), actor)
			want := 403
			if actor == "owner" || actor == "admin" {
				want = 303
			}
			if w := c.call("POST", "/sessions/detach", url.Values{"project": {"demo"}, "grant_id": {id}}); w.Code != want {
				t.Fatalf("status=%d want=%d", w.Code, want)
			}
			if want == 403 && !f.store.Authorize("demo", credential.AccessToken) {
				t.Fatal("denied request revoked grant")
			}
		})
	}
	f := newDeviceFixture(t)
	credential, req := f.approve(t, "owner", "11111111-1111-4111-8111-111111111111", "registered")
	id := reservedGrantID(t, f, "registered")
	b := &reservedGrantBackend{sessions: []Session{{ID: "live", Name: "registered", Harness: req.Harness, SessionID: req.SessionID, ClientID: req.InstanceID}}}
	c := loginPanelUser(t, New(f.store, b, http.NotFoundHandler()), "owner")
	body := c.call("GET", "/projects/sessions?project=demo", nil).Body.String()
	if strings.Contains(body, `name="grant_id"`) {
		t.Fatal("registered grant duplicated")
	}
	for _, values := range []url.Values{
		{"project": {"demo"}, "grant_id": {id}},
		{"project": {"demo"}, "grant_id": {id}, "id": {"live"}},
		{"project": {"demo"}, "grant_id": {"missing"}},
	} {
		if w := c.call("POST", "/sessions/detach", values); w.Code < 400 {
			t.Fatalf("invalid grant target accepted: %d", w.Code)
		}
	}
	for _, p := range f.store.ProjectsFor("owner") {
		for _, token := range p.Tokens {
			if token.Kind == tokenProject {
				if w := c.call("POST", "/sessions/detach", url.Values{"project": {"demo"}, "grant_id": {token.ID}}); w.Code < 400 {
					t.Fatal("parent accepted as grant")
				}
			}
		}
	}
	if !f.store.Authorize("demo", credential.AccessToken) {
		t.Fatal("invalid request revoked live grant")
	}
	b.err = errors.New("backend unavailable")
	if w := c.call("POST", "/sessions/detach", url.Values{"project": {"demo"}, "grant_id": {id}}); w.Code < 400 {
		t.Fatal("release allowed without backend snapshot")
	}
}

func TestReservedGrantsRespectRegistrationExpirationAndPendingRename(t *testing.T) {
	f := newDeviceFixture(t)
	_, req := f.approve(t, "owner", "11111111-1111-4111-8111-111111111111", "reserved")
	id := reservedGrantID(t, f, "reserved")
	p := f.store.data.Projects["demo"]
	for i := range p.Tokens {
		if p.Tokens[i].ID == id {
			p.Tokens[i].PendingNickname = "reserved-next"
		}
	}
	if err := f.store.commitProject(p); err != nil {
		t.Fatal(err)
	}
	rows, err := f.store.reservedSessionGrants("demo", nil)
	if err != nil || len(rows) != 1 || rows[0].PendingNickname != "reserved-next" || rows[0].DisplayState() != "unregistered" {
		t.Fatalf("reserved rows=%+v err=%v", rows, err)
	}
	if got := summarizeSessions(rows); got.Registered != 0 || got.Reserved != 1 || got.Unreachable != 0 {
		t.Fatalf("summary=%+v", got)
	}
	for _, state := range []Session{
		{ID: "live", Name: "reserved", Status: "missing", ErrorCode: "server-restarted", Harness: req.Harness, SessionID: req.SessionID, ClientID: req.InstanceID},
		{ID: "live", Name: "reserved-next", Suspended: true, Harness: req.Harness, SessionID: req.SessionID, ClientID: req.InstanceID},
	} {
		rows, err := f.store.reservedSessionGrants("demo", []Session{state})
		if err != nil || len(rows) != 0 {
			t.Fatalf("registration duplicated: %+v %v", rows, err)
		}
	}
	f.now = f.now.Add(8 * 24 * time.Hour)
	rows, err = f.store.reservedSessionGrants("demo", nil)
	if err != nil || len(rows) != 0 {
		t.Fatalf("expired reservation visible: %+v %v", rows, err)
	}
}

func TestReservedGrantReleaseFailsClosedOnStorageAndCSRF(t *testing.T) {
	f := newDeviceFixture(t)
	_, _ = f.approve(t, "owner", "11111111-1111-4111-8111-111111111111", "reserved")
	id := reservedGrantID(t, f, "reserved")
	b := &reservedGrantBackend{}
	c := loginPanelUser(t, New(f.store, b, http.NotFoundHandler()), "owner")
	req := httptest.NewRequest("POST", "http://broker/sessions/detach", strings.NewReader(url.Values{"project": {"demo"}, "grant_id": {id}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(c.cookie)
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("missing CSRF=%d", w.Code)
	}
	f.store.write = func([]byte) error { return errors.New("storage unavailable") }
	if w := c.call("POST", "/sessions/detach", url.Values{"project": {"demo"}, "grant_id": {id}}); w.Code < 400 {
		t.Fatal("failed write reported success")
	}
	p := f.store.data.Projects["demo"]
	for _, token := range p.Tokens {
		if token.ID == id && token.Revoked {
			t.Fatal("failed write revoked reservation")
		}
	}
}

func TestReservedGrantRowAppearsWithoutRegistration(t *testing.T) {
	f := newDeviceFixture(t)
	f.approve(t, "owner", "11111111-1111-4111-8111-111111111111", "reserved-row")
	c := loginPanelUser(t, New(f.store, &reservedGrantBackend{}, http.NotFoundHandler()), "viewer")
	body := c.call("GET", "/projects/sessions?project=demo", nil).Body.String()
	if !strings.Contains(body, "reserved-row") || !strings.Contains(body, en("session.connection.unregistered")) {
		t.Fatal("unregistered grant row is missing")
	}
	if strings.Contains(body, `name="grant_id"`) {
		t.Fatal("viewer can release grant")
	}
}

func TestReservedGrantReleaseRevokesOnlySelectedGrant(t *testing.T) {
	f := newDeviceFixture(t)
	first, _ := f.approve(t, "owner", "11111111-1111-4111-8111-111111111111", "selected")
	sibling, _ := f.approve(t, "owner", "22222222-2222-4222-8222-222222222222", "sibling")
	id := reservedGrantID(t, f, "selected")
	c := loginPanelUser(t, New(f.store, &reservedGrantBackend{}, http.NotFoundHandler()), "owner")
	c.call("POST", "/sessions/detach", url.Values{"project": {"demo"}, "grant_id": {id}})
	if f.store.Authorize("demo", first.AccessToken) {
		t.Fatal("selected grant still authorizes after release")
	}
	if !f.store.Authorize("demo", sibling.AccessToken) {
		t.Fatal("sibling grant was revoked")
	}
	if _, err := f.store.BeginDeviceAuthorization("demo", f.raw, DeviceRequest{ClientID: "asb-client-v1", Harness: "claude", SessionID: "33333333-3333-4333-8333-333333333333", InstanceID: "pc-example", RequestID: "parent-still-usable"}); err != nil {
		t.Fatalf("parent token was revoked: %v", err)
	}
}

func TestReservedGrantReleaseAllowsSameNicknameApproval(t *testing.T) {
	f := newDeviceFixture(t)
	f.approve(t, "owner", "11111111-1111-4111-8111-111111111111", "reusable")
	id := reservedGrantID(t, f, "reusable")
	c := loginPanelUser(t, New(f.store, &reservedGrantBackend{}, http.NotFoundHandler()), "owner")
	c.call("POST", "/sessions/detach", url.Values{"project": {"demo"}, "grant_id": {id}})
	f.approve(t, "owner", "22222222-2222-4222-8222-222222222222", "reusable")
}

func TestReservedGrantUnauthorizedDeveloperCannotRelease(t *testing.T) {
	f := newDeviceFixture(t)
	credential, _ := f.approve(t, "owner", "11111111-1111-4111-8111-111111111111", "owner-grant")
	id := reservedGrantID(t, f, "owner-grant")
	c := loginPanelUser(t, New(f.store, &reservedGrantBackend{}, http.NotFoundHandler()), "developer")
	w := c.call("POST", "/sessions/detach", url.Values{"project": {"demo"}, "grant_id": {id}})
	if w.Code != http.StatusForbidden {
		t.Fatalf("unauthorized grant release status=%d; want 403", w.Code)
	}
	if !f.store.Authorize("demo", credential.AccessToken) {
		t.Fatal("unauthorized request revoked grant")
	}
}

func TestReservedGrantOldNicknameWithSameIdentityRemainsVisibleAndReleasable(t *testing.T) {
	f := newDeviceFixture(t)
	old, req := f.approve(t, "owner", "11111111-1111-4111-8111-111111111111", "old-nickname")
	oldID := reservedGrantID(t, f, "old-nickname")
	req.RequestID = "same-conversation-new-approval"
	code, err := f.store.BeginDeviceAuthorization("demo", f.raw, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.ApproveDeviceAuthorization("owner", code.UserCode, "current-nickname", false); err != nil {
		t.Fatal(err)
	}
	f.resetPoll()
	current, err := f.store.ExchangeDeviceToken("demo", f.raw, code.DeviceCode, req.ClientID, false)
	if err != nil {
		t.Fatal(err)
	}
	currentID := reservedGrantID(t, f, "current-nickname")
	b := &reservedGrantBackend{sessions: []Session{{ID: "current", Name: "current-nickname", Harness: req.Harness, SessionID: req.SessionID, ClientID: req.InstanceID, Status: "alive", Ready: true}}}
	c := loginPanelUser(t, New(f.store, b, http.NotFoundHandler()), "owner")
	body := c.call("GET", "/projects/sessions?project=demo", nil).Body.String()
	if !strings.Contains(body, `name="grant_id" value="`+oldID+`"`) {
		t.Error("old nickname grant is hidden by same-identity registration")
	}
	if strings.Contains(body, `name="grant_id" value="`+currentID+`"`) {
		t.Error("current nickname grant duplicated as unregistered")
	}
	w := c.call("POST", "/sessions/detach", url.Values{"project": {"demo"}, "grant_id": {oldID}})
	if w.Code != 303 {
		t.Errorf("old nickname grant release=%d; want 303", w.Code)
	}
	if f.store.Authorize("demo", old.AccessToken) {
		t.Error("old nickname grant still authorizes")
	}
	if !f.store.Authorize("demo", current.AccessToken) {
		t.Error("same-identity current grant was revoked")
	}
	if len(b.detached) != 0 {
		t.Error("same-identity current registration was detached")
	}
	f.approve(t, "owner", "22222222-2222-4222-8222-222222222222", "old-nickname")
}
