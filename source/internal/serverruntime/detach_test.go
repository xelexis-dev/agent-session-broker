package serverruntime

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
)

func TestManagedDetachReleasesNicknameAndRevokesOldAdmission(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "UI"
		if legacy {
			name = "existing-detached-after-restart"
		}
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			store, err := controlpanel.OpenSQLite(filepath.Join(directory, "control"))
			if err != nil {
				t.Fatal(err)
			}
			const password = "test-only-detach-password"
			if err := store.Setup(password); err != nil {
				t.Fatal(err)
			}
			if err := store.CreateProject("test"); err != nil {
				t.Fatal(err)
			}
			for _, role := range []string{"owner", "viewer"} {
				if err := store.CreateUser("admin", role, password, "user"); err != nil {
					t.Fatal(err)
				}
				if err := store.SetProjectMember("admin", "test", role, role); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := store.IssueToken("test", "reusable")
			if err != nil {
				t.Fatal(err)
			}
			tokenID := store.Projects()[0].Tokens[0].ID
			otherRaw, err := store.IssueToken("test", "other")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			h, closeHandler, err := newManagedHandler(directory, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { closeHandler() })
			bound := func(token string) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					r.Header.Set("Authorization", "Bearer "+token)
					h.ServeHTTP(w, r)
				})
			}
			request := protectedRequest(1, "ignored")
			first := managedTool[broker.RegisterResult](t, bound(raw), "register_session", request)
			if first.Code != "" {
				t.Fatal(first.Code)
			}
			other := managedTool[broker.RegisterResult](t, bound(otherRaw), "register_session", protectedRequest(3, "ignored"))
			if other.Code != "" {
				t.Fatal(other.Code)
			}
			otherLease := "lease-3"
			if legacy {
				managedTool[map[string]bool](t, bound(raw), "stop_session", map[string]string{"name": first.Session.Name})
				if err := closeHandler(); err != nil {
					t.Fatal(err)
				}
				h, closeHandler, err = newManagedHandler(directory, nil)
				if err != nil {
					t.Fatal(err)
				}
				r := protectedRequest(3, "ignored")
				otherLease = "lease-other-resumed"
				resumed := managedTool[broker.RegisterResult](t, bound(otherRaw), "resume_session", broker.ResumeRequest{RegistrationID: other.Session.RegistrationID, Harness: r.Harness, SessionID: r.SessionID, PID: r.PID, CWD: r.CWD, ClientID: r.ClientID, ChannelID: otherLease, Secret: r.Secret, RequestID: "resume-other", ExpectedRevision: other.Session.Revision})
				if resumed.Code != "" {
					t.Fatal("other session restart resume", resumed.Code)
				}
			}
			var cookie *http.Cookie
			var csrf string
			call := func(method, path string, values url.Values) *httptest.ResponseRecorder {
				if values == nil {
					values = url.Values{}
				}
				if method == "POST" {
					values.Set("csrf", csrf)
				}
				r := httptest.NewRequest(method, "http://broker"+path, strings.NewReader(values.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				if cookie != nil {
					r.AddCookie(cookie)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}
			csrfFrom := func(w *httptest.ResponseRecorder) string {
				m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
				if len(m) != 2 {
					t.Fatal("missing CSRF")
				}
				return m[1]
			}
			loginAs := func(user string) {
				cookie = nil
				login := call("GET", "/login", nil)
				cookie, csrf = login.Result().Cookies()[0], csrfFrom(login)
				login = call("POST", "/login", url.Values{"username": {user}, "password": {password}})
				if login.Code != 303 {
					t.Fatal("login", login.Code)
				}
				cookie = login.Result().Cookies()[0]
			}
			if legacy {
				for _, user := range []string{"viewer", "owner"} {
					loginAs(user)
					page := call("GET", "/projects/sessions?project=test", nil)
					if page.Code != 200 {
						t.Fatal("read-only listing", page.Code)
					}
					// Explicit repair may create a fresh registration, but a read
					// must never revoke the caller's still-valid admission token.
					request.ChannelID, request.RequestID = "repair-"+user, "repair-"+user
					repaired := managedTool[broker.RegisterResult](t, bound(raw), "register_session", request)
					if repaired.Code != "" || repaired.Session.Name != "reusable" {
						t.Fatal("same-token explicit registration after listing", repaired.Code)
					}
					managedTool[map[string]bool](t, bound(raw), "stop_session", map[string]string{"name": repaired.Session.Name})
				}
			}
			loginAs("admin")
			page := call("GET", "/projects/sessions?project=test", nil)
			csrf = csrfFrom(page)
			if !legacy {
				w := call("POST", "/sessions/detach", url.Values{"project": {"test"}, "id": {first.Session.RegistrationID}})
				if w.Code != 303 {
					t.Fatal("detach", w.Code)
				}
				page = call("GET", "/projects/sessions?project=test", nil)
			} else {
				// Historical detach and MCP stop are indistinguishable. Preserve
				// admission until its owner explicitly cancels the reservation.
				if w := call("POST", "/tokens/revoke", url.Values{"project": {"test"}, "id": {tokenID}}); w.Code != 303 {
					t.Fatal("explicit cancellation", w.Code)
				}
				page = call("GET", "/projects/sessions?project=test", nil)
			}
			if strings.Contains(page.Body.String(), "<strong>reusable</strong>") || !strings.Contains(page.Body.String(), "<strong>other</strong>") {
				t.Error("detached registration or its token is still listed")
			}
			// Revoking one token may interrupt in-flight requests, but the other
			// session must remain usable with its own credential and lease.
			managedTool[broker.SendResult](t, bound(otherRaw), "send_message", map[string]string{"from": "other", "to": "other", "body": "still connected"})
			message := managedTool[broker.Message](t, bound(otherRaw), "wait_for_message", map[string]string{"recipient": "other", "channelId": otherLease})
			if message.Body != "still connected" {
				t.Fatal("other session lost delivery")
			}
			// Old credentials must fail before a replacement is issued, including
			// the auto-register path an old client could use after not_found.
			w := httptest.NewRecorder()
			bound(raw).ServeHTTP(w, httptest.NewRequest("GET", "https://broker/mcp?project=test", nil))
			if w.Code != 401 {
				t.Error("old admission remains valid", w.Code)
			}
			// The legacy issuance form is gone; reissue through the store while
			// the handler is closed, exactly as an operator script would.
			if err := closeHandler(); err != nil {
				t.Fatal(err)
			}
			store, err = controlpanel.OpenSQLite(filepath.Join(directory, "control"))
			if err != nil {
				t.Fatal(err)
			}
			reissued, err := store.IssueToken("test", "reusable")
			if err != nil {
				t.Fatal("nickname cannot be reissued", err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			h, closeHandler, err = newManagedHandler(directory, nil)
			if err != nil {
				t.Fatal(err)
			}
			second := managedTool[broker.RegisterResult](t, bound(reissued), "register_session", protectedRequest(2, "ignored"))
			if second.Code != "" || second.Session.Name != "reusable" || second.Session.RegistrationID == first.Session.RegistrationID {
				t.Fatal("replacement did not acquire the released nickname", second.Code)
			}
			if err := closeHandler(); err != nil {
				t.Fatal(err)
			}
			h, closeHandler, err = newManagedHandler(directory, nil)
			if err != nil {
				t.Fatal(err)
			}
			list := managedTool[[]broker.Session](t, bound(reissued), "list_sessions", map[string]any{})
			if len(list) != 2 || list[1].RegistrationID != second.Session.RegistrationID {
				t.Fatal("removed registration returned after restart")
			}
		})
	}
}
