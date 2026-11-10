package controlpanel

import (
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestRound2F1PendingApprovalRechecksParentAndProject(t *testing.T) {
	for _, action := range []string{"revoke", "disable", "delete"} {
		t.Run(action, func(t *testing.T) {
			s, b, h := round2Fixture(t)
			code := misuseDevice(t, s, "demo", "owner")
			c := loginPanelUser(t, h, "owner")
			if w := c.call("GET", "/auth/code?code="+url.QueryEscape(code.UserCode), nil); w.Code != 200 {
				t.Fatal("approval fixture unavailable")
			}
			path := "/projects/save"
			form := url.Values{"name": {"demo"}}
			if action == "revoke" {
				path = "/tokens/revoke"
				form = url.Values{"project": {"demo"}, "id": {s.ProjectsFor("owner")[0].Tokens[0].ID}}
			}
			if action == "delete" {
				path = "/projects/delete"
				form = url.Values{"project": {"demo"}, "confirmName": {"demo"}}
			}
			if w := c.call("POST", path, form); w.Code != 303 {
				t.Fatalf("invalidation=%d", w.Code)
			}
			before := misuseSnapshot(t, s)
			calls := len(b.mutations)
			w := c.call("POST", "/auth/code", url.Values{"code": {code.UserCode}, "action": {"approve"}, "nickname": {"stale-approval"}})
			if w.Code != 404 {
				t.Fatalf("stale approval=%d", w.Code)
			}
			if misuseSnapshot(t, s) != before || len(b.mutations) != calls {
				t.Fatal("stale approval created a grant or backend operation")
			}
		})
	}
}
func TestRound2F2FixedAdminAndUnsupportedDeletion(t *testing.T) {
	s, b, h := round2Fixture(t)
	c := loginPanelUser(t, h, "admin")
	before := misuseSnapshot(t, s)
	for _, form := range []url.Values{{"id": {"admin"}, "role": {"user"}, "enabled": {"true"}}, {"id": {"admin"}, "role": {"admin"}}, {"id": {"admin"}, "role": {"user"}}} {
		if w := c.call("POST", "/admin/users/update", form); w.Code != 400 {
			t.Fatalf("last admin change=%d", w.Code)
		}
	}
	for _, path := range []string{"/admin/users/delete", "/profile/delete"} {
		if w := c.call("POST", path, url.Values{"id": {"admin"}}); w.Code != 404 {
			t.Fatalf("unsupported deletion=%d", w.Code)
		}
	}
	assertMisuseUnchanged(t, s, b.misuseBackend, before)
	admin, ok := s.User("admin")
	if !ok || !admin.Enabled || admin.Role != "admin" {
		t.Fatal("lost fixed admin")
	}
}
func TestRound2F2PasswordChangeRejectsOtherLoginForms(t *testing.T) {
	s, b, h := round2Fixture(t)
	first := loginPanelUser(t, h, "admin")
	second := loginPanelUser(t, h, "admin")
	other := loginPanelUser(t, h, "owner")
	next := "round two synthetic password"
	w := first.call("POST", "/profile/password", url.Values{"currentPassword": {testPassword}, "newPassword": {next}, "confirmPassword": {next}})
	if w.Code != 303 || w.Header().Get("Location") != "/login" {
		t.Fatalf("password change=%d", w.Code)
	}
	before := misuseSnapshot(t, s)
	for _, c := range []*userPanelClient{first, second} {
		w := c.call("POST", "/projects/create", url.Values{"name": {"stale-login"}})
		if w.Code != 303 || w.Header().Get("Location") != "/login" {
			t.Fatal("old login could submit form")
		}
	}
	assertMisuseUnchanged(t, s, b.misuseBackend, before)
	if w := other.call("GET", "/profile", nil); w.Code != 200 {
		t.Fatal("unrelated login invalidated")
	}
	if _, ok := s.AuthenticateUser("admin", testPassword); ok {
		t.Fatal("old password accepted")
	}
	if _, ok := s.AuthenticateUser("admin", next); !ok {
		t.Fatal("new password rejected")
	}
}
func concurrentRound2(t *testing.T, calls ...func() int) []int {
	t.Helper()
	start := make(chan struct{})
	codes := make(chan int, len(calls))
	var wg sync.WaitGroup
	for _, call := range calls {
		wg.Go(func() { <-start; codes <- call() })
	}
	close(start)
	wg.Wait()
	close(codes)
	var out []int
	for code := range codes {
		out = append(out, code)
	}
	return out
}
func TestRound2F3ConcurrentSettingsAndMembers(t *testing.T) {
	s, _, h := round2Fixture(t)
	if err := s.CreateUser("admin", "admin2", testPassword, "admin"); err != nil {
		t.Fatal(err)
	}
	a := loginPanelUser(t, h, "admin")
	b := loginPanelUser(t, h, "admin2")
	for range 8 {
		codes := concurrentRound2(t,
			func() int {
				return a.call("POST", "/projects/save", url.Values{"name": {"demo"}, "enabled": {"true"}}).Code
			},
			func() int { return b.call("POST", "/projects/save", url.Values{"name": {"demo"}}).Code },
			func() int {
				return a.call("POST", "/projects/members", url.Values{"project": {"demo"}, "username": {"developer"}, "role": {"viewer"}, "action": {"update"}}).Code
			},
			func() int {
				return b.call("POST", "/projects/members", url.Values{"project": {"demo"}, "username": {"viewer"}, "role": {"developer"}, "action": {"update"}}).Code
			})
		for _, code := range codes {
			if code != 303 {
				t.Fatalf("concurrent save=%d", code)
			}
		}
		if s.ProjectRole("developer", "demo") != "viewer" || s.ProjectRole("viewer", "demo") != "developer" {
			t.Fatal("independent member updates lost")
		}
		s.mu.RLock()
		valid := validState(s.data)
		s.mu.RUnlock()
		if !valid {
			t.Fatal("concurrent saves broke state")
		}
	}
	// There is no optimistic revision field: a valid stale form is last-write-wins.
	a.call("GET", "/projects/settings?project=demo", nil)
	b.call("POST", "/projects/save", url.Values{"name": {"demo"}})
	if w := a.call("POST", "/projects/save", url.Values{"name": {"demo"}, "enabled": {"true"}}); w.Code != 303 {
		t.Fatal("stale save unexpectedly rejected")
	}
	if !s.ProjectsFor("owner")[0].Enabled {
		t.Fatal("last completed save not retained")
	}
}
func TestRound2F3ConcurrentLastOwnerDemotions(t *testing.T) {
	s, _, h := round2Fixture(t)
	if err := s.CreateUser("admin", "admin2", testPassword, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectMember("admin", "demo", "developer", "owner"); err != nil {
		t.Fatal(err)
	}
	a := loginPanelUser(t, h, "admin")
	b := loginPanelUser(t, h, "admin2")
	codes := concurrentRound2(t,
		func() int {
			return a.call("POST", "/projects/members", url.Values{"project": {"demo"}, "username": {"owner"}, "role": {"viewer"}, "action": {"update"}}).Code
		},
		func() int {
			return b.call("POST", "/projects/members", url.Values{"project": {"demo"}, "username": {"developer"}, "role": {"viewer"}, "action": {"update"}}).Code
		})
	success, rejected := 0, 0
	for _, code := range codes {
		if code == 303 {
			success++
		} else if code == 400 {
			rejected++
		} else {
			t.Fatalf("demotion=%d", code)
		}
	}
	if success != 1 || rejected != 1 {
		t.Fatal("last owner removal was not serialized")
	}
	owners := 0
	for _, role := range s.ProjectsFor("admin")[0].Members {
		if role == "owner" {
			owners++
		}
	}
	if owners != 1 {
		t.Fatal("incorrect owner count after concurrent demotions")
	}
}
func TestRound2F4LogoutRejectsEveryOpenMutationForm(t *testing.T) {
	s, b, h := round2Fixture(t)
	tab := loginPanelUser(t, h, "admin")
	independent := loginPanelUser(t, h, "admin")
	if w := tab.call("POST", "/logout", nil); w.Code != 303 {
		t.Fatal("logout failed")
	}
	before := misuseSnapshot(t, s)
	for path, kind := range misuseRoutes {
		if kind != "post" && kind != "approval" {
			continue
		}
		w := misuseRequest(h, tab.cookie, "POST", path, url.Values{"csrf": {tab.csrf}, "project": {"demo"}, "id": {"own"}, "action": {"approve"}})
		if w.Code != 303 || w.Header().Get("Location") != "/login" {
			t.Fatalf("stale tab on %s=%d", path, w.Code)
		}
	}
	assertMisuseUnchanged(t, s, b.misuseBackend, before)
	if w := independent.call("GET", "/profile", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "</html>") {
		t.Fatal("separate login invalidated by tab logout")
	}
}
