package controlpanel

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

const misuseHTML = `<svg/onload=alert(17)>`

func assertMisuseEscaped(t *testing.T, body string) {
	t.Helper()
	if strings.Contains(body, misuseHTML) {
		t.Fatal("unescaped user HTML")
	}
	if !strings.Contains(body, html.EscapeString(misuseHTML)) {
		t.Fatal("payload not exercised in rendered text")
	}
	if !strings.Contains(body, "</html>") {
		t.Fatal("template incomplete")
	}
}

func TestMisuseC6StoredTextAcrossPages(t *testing.T) {
	s, b, h := misuseFixture(t)
	if _, err := s.IssueProjectTokenFor("owner", "demo", misuseHTML); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateProfile("owner", misuseHTML, "owner@example.test"); err != nil {
		t.Fatal(err)
	}
	b.rows["demo"][0].Name = misuseHTML
	// The panel receives session names from its backend; it must escape even a
	// value accepted by an older server. Broker rename validation is section A.
	b.rows["demo"][0].ErrorCode = misuseHTML
	input := bugreport.Input{Title: misuseHTML, Summary: misuseHTML, Steps: misuseHTML, Expected: misuseHTML, Actual: misuseHTML, Tool: misuseHTML, Client: &bugreport.ClientDiagnostics{ClientVersion: misuseHTML, ServerVersion: misuseHTML, OS: misuseHTML, Arch: misuseHTML, AuthState: misuseHTML, LastFollow: misuseHTML, Events: []bugreport.DiagnosticEvent{{At: time.Now(), Event: misuseHTML, ReasonClass: misuseHTML}}}}
	report, err := b.reports.Create(input, bugreport.Reporter{ProjectGenerationID: s.ProjectGeneration("demo"), Project: "demo", SessionName: misuseHTML, RegistrationID: "synthetic-report-reg", Harness: "claude", OwnerID: "owner"}, misuseHTML)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetScopeIssuanceReady("admin", true); err != nil {
		t.Fatal(err)
	}
	// Seed a real audit event through the store's normal persistence boundary.
	s.mu.Lock()
	now := s.now()
	err = s.commitAudited(s.data, []AuditEvent{{Version: 1, Project: "demo", Kind: "detached", OwnerID: "owner", At: now, Management: &protocol.ManagementEvent{Kind: "detached", ActorName: misuseHTML, TargetName: misuseHTML, TargetRegistrationID: "synthetic-audit-reg", TargetParentLabel: misuseHTML, At: now}}})
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	owner := loginPanelUser(t, h, "owner")
	admin := loginPanelUser(t, h, "admin")
	for _, tc := range []struct {
		c    *userPanelClient
		path string
	}{{owner, "/"}, {owner, "/profile"}, {owner, "/projects/settings?project=demo"}, {owner, "/projects/sessions?project=demo"}, {owner, "/projects/audit?project=demo"}, {admin, "/admin/bug-reports"}, {admin, "/admin/bug-reports/detail?id=" + url.QueryEscape(report.ID)}} {
		t.Run(tc.path, func(t *testing.T) {
			w := tc.c.call("GET", tc.path, nil)
			if w.Code != 200 {
				t.Fatalf("page=%d", w.Code)
			}
			assertMisuseEscaped(t, w.Body.String())
		})
	}
	live := owner.call("GET", "/projects/sessions/live?project=demo", nil)
	if live.Code != 200 || strings.Contains(live.Body.String(), misuseHTML) || !strings.Contains(live.Body.String(), `\u003csvg`) {
		t.Fatal("live JSON is not safely encoded")
	}
}
func TestMisuseC6RejectedProjectAndReflectedForms(t *testing.T) {
	s, _, h := misuseFixture(t)
	c := loginPanelUser(t, h, "owner")
	before := misuseSnapshot(t, s)
	w := c.call("POST", "/projects/create", url.Values{"name": {misuseHTML}})
	if w.Code != 400 {
		t.Fatalf("invalid project=%d", w.Code)
	}
	assertMisuseEscaped(t, w.Body.String())
	if misuseSnapshot(t, s) != before {
		t.Fatal("invalid project persisted")
	}
	code := misuseDevice(t, s, "demo", "owner")
	w = c.call("GET", "/auth/code?code="+url.QueryEscape(code.UserCode)+"&name="+url.QueryEscape(misuseHTML), nil)
	if w.Code != 200 {
		t.Fatalf("approval preview=%d", w.Code)
	}
	assertMisuseEscaped(t, w.Body.String())
	// A script-shaped nickname is allowed as plain text; approval must not make it markup.
	w = c.call("POST", "/auth/code", url.Values{"code": {code.UserCode}, "action": {"approve"}, "nickname": {misuseHTML}})
	if w.Code != 303 {
		t.Fatalf("nickname approval=%d", w.Code)
	}
	w = c.call("GET", "/projects/sessions?project=demo", nil)
	if w.Code != 200 {
		t.Fatal("approved nickname page unavailable")
	}
	assertMisuseEscaped(t, w.Body.String())
	w = c.call("POST", "/tokens/project", url.Values{"project": {"demo"}, "label": {misuseHTML}, "scope_selection": {"invalid"}})
	if w.Code != 400 {
		t.Fatalf("invalid token form=%d", w.Code)
	}
	assertMisuseEscaped(t, w.Body.String())
	// Setup and login retain username/form input only through html/template.
	get := misuseRequest(h, nil, "GET", "/login", nil)
	cookie := get.Result().Cookies()[0]
	w = misuseRequest(h, cookie, "POST", "/login", url.Values{"csrf": {cookie.Value}, "username": {misuseHTML}, "password": {"wrong"}})
	if w.Code != 401 {
		t.Fatal("invalid login not rejected")
	}
	assertMisuseEscaped(t, w.Body.String())
}

func TestMisuseC6EveryHTMLViewEscapesProjection(t *testing.T) {
	// Defense in depth for historically stored data that validation now rejects.
	// Every template view is exercised, including empty/read-only surfaces whose
	// usual handler cannot create a malicious project/user identifier.
	for _, view := range []string{"setup", "login", "projects", "settings", "sessions", "users", "user-detail", "admin-settings", "profile", "authcode", "audit", "bug-reports", "bug-report-detail"} {
		t.Run(view, func(t *testing.T) {
			p := page{View: view, Error: misuseHTML, Form: url.Values{"body": {misuseHTML}}, AnnouncementComplete: true, AnnouncementResults: []AnnouncementResult{{Project: misuseHTML, Name: misuseHTML, Error: misuseHTML}}, CanManage: map[string]bool{}, Roles: map[string]string{}, User: User{ID: "admin", Username: "admin", Role: "admin", Enabled: true}, Projects: []Project{{Name: misuseHTML}}, Selected: Project{Name: misuseHTML, Mode: "token"}, Project: misuseHTML, CanEdit: true, CanIssue: true, SelectedUser: User{Username: misuseHTML}, Device: &DeviceView{Project: misuseHTML, InstanceID: misuseHTML, SessionID: misuseHTML}, ProfileProjects: []profileProject{{Name: misuseHTML, Role: "Owner"}}}
			w := httptest.NewRecorder()
			render(w, http.StatusBadRequest, p)
			assertMisuseEscaped(t, w.Body.String())
		})
	}
}
