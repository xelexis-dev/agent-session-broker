package controlpanel

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestProjectTokenViewsCrossCheckLiveSessionsAndKeepHistory(t *testing.T) {
	now := time.Date(2026, 9, 16, 2, 0, 0, 0, time.UTC)
	live := Session{Harness: "claude", SessionID: "conversation-1", ClientID: "pc-1", Status: "alive"}
	tokens := []SessionToken{
		{Kind: tokenProject, ID: "parent", Label: "home", LastUsedAt: now.Add(-2 * time.Hour), Approvals: 2, LastRegisteredAt: now.Add(-time.Hour)},
		{Kind: tokenSession, ID: "grant-live", ParentID: "parent", BoundIdentity: sessionIdentity(live), LastUsedAt: now},
		{Kind: tokenSession, ID: "grant-revoked", ParentID: "parent", BoundIdentity: "other", Revoked: true, LastUsedAt: now.Add(time.Hour)},
		{Kind: tokenProject, ID: "quiet", Label: "mac"},
		{Kind: tokenProject, ID: "revoked", Revoked: true},
		{Nickname: "legacy"},
	}
	all := func(SessionToken) bool { return true }
	views := projectTokenViews(tokens, []Session{live}, all)
	if len(views) != 2 || views[0].ID != "parent" || views[1].ID != "quiet" {
		t.Fatalf("views = %+v", views)
	}
	if views[0].State != "active" || views[0].Live != 1 || !views[0].LastUsed.Equal(now.Add(time.Hour)) || views[0].Approvals != 2 {
		t.Fatalf("live token view = %+v", views[0])
	}
	if views[1].State != "idle" || views[1].Live != 0 || !views[1].LastUsed.IsZero() {
		t.Fatalf("unused token view = %+v", views[1])
	}
	missing := live
	missing.Status = "missing"
	if views = projectTokenViews(tokens, []Session{missing}, all); views[0].State != "registered" || views[0].Live != 0 {
		t.Fatalf("missing session counted as connected: %+v", views[0])
	}
	detached := live
	detached.ErrorCode = "detached"
	if views = projectTokenViews(tokens, []Session{detached}, all); views[0].State != "registered" {
		t.Fatalf("detached session counted as connected: %+v", views[0])
	}
	if views = projectTokenViews(tokens, nil, func(token SessionToken) bool { return token.ID == "quiet" }); len(views) != 1 || views[0].ID != "quiet" {
		t.Fatalf("visibility filter ignored: %+v", views)
	}
}

type tokenViewBackend struct{ sessions []Session }

func (b tokenViewBackend) Sessions(context.Context, string) ([]Session, error) {
	return b.sessions, nil
}
func (b tokenViewBackend) Detach(context.Context, string, string) error { return nil }

// seedApprovedGrant runs one conversation through the real project-token flow
// and confirms its registration, returning the parent token id.
func seedApprovedGrant(t *testing.T, s *Store, owner, label string, req DeviceRequest, nickname string) string {
	t.Helper()
	raw, err := s.IssueProjectTokenFor(owner, "demo", label)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := s.BeginDeviceAuthorization("demo", raw, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApproveDeviceAuthorization(owner, issued.UserCode, nickname, false); err != nil {
		t.Fatal(err)
	}
	granted, err := s.ExchangeDeviceToken("demo", raw, issued.DeviceCode, "asb-client-v1", false)
	if err != nil {
		t.Fatal(err)
	}
	grant, _, ok := s.authorize("demo", granted.AccessToken)
	if !ok {
		t.Fatal("grant unauthorized")
	}
	if err = grant.Connected(req.identity()); err != nil {
		t.Fatal(err)
	}
	for _, token := range s.ProjectsFor(owner)[0].Tokens {
		if token.Kind == tokenProject && token.Label == label {
			return token.ID
		}
	}
	t.Fatal("parent token missing")
	return ""
}

func TestProjectSettingsListsProjectTokensOnly(t *testing.T) {
	s := userStore(t)
	if _, err := s.IssueTokenFor("owner", "demo", "legacy-nick", "owner"); err != nil {
		t.Fatal(err)
	}
	req := DeviceRequest{ClientID: "asb-client-v1", Harness: "claude", SessionID: "conversation-1", InstanceID: "pc-1", RequestID: "request-1"}
	seedApprovedGrant(t, s, "owner", "home", req, "home-claude-1")
	if _, err := s.IssueProjectTokenFor("developer", "demo", "dev-token"); err != nil {
		t.Fatal(err)
	}
	connected := Session{ID: "reg-1", Name: "home-claude-1", Harness: req.Harness, SessionID: req.SessionID, ClientID: req.InstanceID, Status: "alive", Ready: true}
	h := New(s, tokenViewBackend{sessions: []Session{connected}}, http.NotFoundHandler())
	owner := loginPanelUser(t, h, "owner")
	settings := owner.call("GET", "/projects/settings?project=demo", nil)
	body := settings.Body.String()
	for _, want := range []string{en("tokens.project_tokens"), ">home<", ">dev-token<", en("tokens.approval_count", "n", 1), en("tokens.last_registered"), `action="/tokens/project"`, `data-confirm-action="project-token-revoke"`, `data-confirm-target="home"`} {
		if !strings.Contains(body, want) {
			t.Errorf("settings missing %s", want)
		}
	}
	for _, absent := range []string{"legacy-nick", `action="/tokens/issue"`, `name="nickname"`, "세션 토큰 예약 목록"} {
		if strings.Contains(body, absent) {
			t.Errorf("settings still shows %s", absent)
		}
	}
	table := layoutTables(t, body)[en("tokens.project_tokens")]
	if len(table.headers) != 7 || len(table.rows) != 2 {
		t.Fatalf("token table = %+v", table)
	}
	if state := table.rows[0][5].text; !strings.Contains(state, en("tokens.active")) || !strings.Contains(state, en("tokens.connected_count", "n", 1)) {
		t.Fatalf("connected token state = %q", state)
	}
	if !strings.Contains(table.rows[1][5].text, en("tokens.waiting")) {
		t.Fatalf("unused token state = %q", table.rows[1][5].text)
	}
	detail := owner.call("GET", "/projects/sessions?project=demo", nil).Body.String()
	if strings.Contains(detail, en("tokens.project_tokens")) || strings.Contains(detail, `action="/tokens/project"`) || !strings.Contains(detail, en("sessions.caption")) {
		t.Error("detail page still carries token management")
	}
	if w := owner.call("POST", "/tokens/issue", url.Values{"project": {"demo"}, "nickname": {"again"}}); w.Code != http.StatusNotFound {
		t.Errorf("legacy issue route = %d", w.Code)
	}
	developer := loginPanelUser(t, h, "developer")
	body = developer.call("GET", "/projects/settings?project=demo", nil).Body.String()
	if strings.Contains(body, ">home<") || !strings.Contains(body, ">dev-token<") {
		t.Error("developer sees other owners' tokens")
	}
	viewer := loginPanelUser(t, h, "viewer")
	body = viewer.call("GET", "/projects/settings?project=demo", nil).Body.String()
	if strings.Contains(body, `action="/tokens/project"`) || len(layoutTables(t, body)[en("tokens.project_tokens")].headers) != 6 {
		t.Error("viewer has issuance controls")
	}
	offline := New(s, tokenViewBackend{}, http.NotFoundHandler())
	body = loginPanelUser(t, offline, "owner").call("GET", "/projects/settings?project=demo", nil).Body.String()
	if state := layoutTables(t, body)[en("tokens.project_tokens")].rows[0][5].text; strings.Contains(state, en("tokens.active")) || !strings.Contains(state, en("tokens.registered")) {
		t.Errorf("state without a live session = %q", state)
	}
}
