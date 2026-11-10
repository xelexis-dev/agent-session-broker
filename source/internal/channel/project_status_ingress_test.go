package channel

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestResumedClaudeProjectRefusalAuthStatusTool(t *testing.T) {
	for _, code := range []string{"project_disabled", "project_deleting"} {
		t.Run(code, func(t *testing.T) {
			r := newDetachedRuntime(t)
			target, _ := url.Parse(r.endpoint)
			target.Path, target.RawQuery = "", ""
			reverse := httputil.NewSingleHostReverseProxy(target)
			var refused atomic.Bool
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				if refused.Load() && (q.URL.Path == "/mcp" || q.URL.Path == "/oauth/transport-policy") {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Retry-After", "30")
					w.WriteHeader(403)
					w.Write([]byte(`{"error":"` + code + `"}`))
					return
				}
				reverse.ServeHTTP(w, q)
			}))
			defer proxy.Close()
			r.endpoint = proxy.URL + "/mcp?project=detach"
			id := protocol.AttachRequest{Harness: "claude", SessionID: "11111111-1111-4111-8111-111111119931", PID: 9931, CWD: "/synthetic/project-status"}
			dir := t.TempDir()
			h := r.identityHarness(t, dir, id, handoverOptions)
			h.attach(t)
			first := h.status(t)
			if !first.Registered {
				t.Fatal("initial registration missing")
			}
			h.close()
			temporary := claudeHandoverIdentity(9932, id.PID)
			temporary.CWD = id.CWD
			state, err := clientstate.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			cid, err := state.ClientID()
			if err != nil {
				t.Fatal(err)
			}
			scope := brokerScope(r.endpoint, "detach")
			pending := clientstate.SessionState{Scope: scope, Project: "detach", Harness: "claude", SessionID: temporary.SessionID, ClientID: cid, Registration: clientstate.Registration{Secret: clientstate.NewSecret(), InitialAttempt: initialPending}}
			if err := state.SaveSession(clientstate.SessionKey(scope, "claude", temporary.SessionID, cid), pending); err != nil {
				t.Fatal(err)
			}
			roster := &followRoster{id: temporary}
			h = r.identityHarness(t, dir, temporary, followOptions(roster))
			waitFor(t, "temporary registered", func() bool { return h.status(t).Registered })
			roster.set(id)
			waitFor(t, "same registration followed", func() bool {
				s := h.status(t)
				return s.Registered && s.RegistrationID == first.RegistrationID && strings.Contains(s.Message, "followed: resumed")
			})
			jar, _ := cookiejar.New(nil)
			browser := &http.Client{Jar: jar, CheckRedirect: noRedirect}
			csrf := ""
			request := func(path string, form url.Values) {
				t.Helper()
				var response *http.Response
				var err error
				if form == nil {
					response, err = browser.Get(proxy.URL + path)
				} else {
					form.Set("csrf", csrf)
					response, err = browser.PostForm(proxy.URL+path, form)
				}
				if err != nil {
					t.Fatal("fixture panel request failed")
				}
				defer response.Body.Close()
				body, _ := io.ReadAll(response.Body)
				if response.StatusCode != 200 && response.StatusCode != 303 {
					t.Fatalf("fixture panel status %d", response.StatusCode)
				}
				if match := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindSubmatch(body); len(match) == 2 {
					csrf = string(match[1])
				}
			}
			request("/login", nil)
			request("/login", url.Values{"username": {"admin"}, "password": {"synthetic-detached-password"}})
			request("/projects/settings?project=detach", nil)
			request("/projects/save", url.Values{"name": {"detach"}, "mode": {"token"}})
			if code == "project_deleting" {
				refused.Store(true)
			}
			waitFor(t, "refused reconnect diagnostic", func() bool {
				s := h.status(t)
				return s.Diagnostics != nil && s.Diagnostics.LastFailureCode == strings.ReplaceAll(code, "_", "-") && s.Diagnostics.LastFailureReason == "reconnect-failed"
			})
			status := h.status(t)
			if !strings.Contains(status.Message, code) {
				t.Errorf("ingress auth_status message omits %s", code)
			}
			refused.Store(false)
			request("/projects/settings?project=detach", nil)
			request("/projects/save", url.Values{"name": {"detach"}, "mode": {"token"}, "enabled": {"true"}})
			waitFor(t, "recovered auth_status", func() bool { s := h.status(t); return s.Registered && !strings.Contains(s.Message, code) })
		})
	}
}
