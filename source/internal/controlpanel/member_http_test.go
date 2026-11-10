package controlpanel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func memberPanelFixture(t *testing.T) (*Store, http.Handler) {
	t.Helper()
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"owner", "dev", "viewer", "outsider", "alice", "malice", "alice-off"} {
		if err := s.CreateUser("admin", name, testPassword, "user"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateProjectFor("owner", "demo"); err != nil {
		t.Fatal(err)
	}
	for name, role := range map[string]string{"dev": "developer", "viewer": "viewer"} {
		if err := s.SetProjectMember("owner", "demo", name, role); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpdateUser("admin", "alice-off", "user", false, ""); err != nil {
		t.Fatal(err)
	}
	return s, New(s, &panelBackend{}, http.NotFoundHandler())
}

func TestMemberSuggestionsPermissionsAndPrivacy(t *testing.T) {
	s, h := memberPanelFixture(t)
	if err := s.UpdateProfile("alice", "Private Display Name", "private@example.test"); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []string{"owner", "admin", "dev", "viewer", "outsider"} {
		c := loginPanelUser(t, h, actor)
		w := c.call("GET", "/projects/members/suggestions?project=demo&q=ALICE", nil)
		if actor != "owner" && actor != "admin" {
			if w.Code != 403 {
				t.Fatalf("%s suggestions = %d", actor, w.Code)
			}
			continue
		}
		if w.Code != 200 {
			t.Fatalf("%s suggestions = %d", actor, w.Code)
		}
		var result map[string][]string
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result) != 1 || strings.Join(result["usernames"], ",") != "alice,malice" {
			t.Fatalf("private/incorrect response: %s", w.Body)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("suggestions cached")
		}
		if got := c.call("GET", "/projects/members/suggestions?project=missing&q=alice", nil).Code; got != 403 {
			t.Fatalf("missing project = %d", got)
		}
		if got := c.call("POST", "/projects/members/suggestions?project=demo&q=alice", nil).Code; got != 405 {
			t.Fatalf("POST suggestions = %d", got)
		}
		if got := c.call("GET", "/projects/members/suggestions?project=demo&q="+strings.Repeat("a", 65), nil).Code; got != 400 {
			t.Fatalf("long query = %d", got)
		}
		for _, q := range []string{"", "dev", "%3Cscript%3E"} {
			w := c.call("GET", "/projects/members/suggestions?project=demo&q="+q, nil)
			if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"usernames":[]}` {
				t.Fatalf("query %s = %d %s", q, w.Code, w.Body)
			}
		}
	}
	anonymous := &userPanelClient{t: t, h: h}
	if w := anonymous.call("GET", "/projects/members/suggestions?project=demo&q=alice", nil); w.Code != 303 || w.Header().Get("Location") != "/login" {
		t.Fatal("anonymous lookup accepted")
	}
	c := loginPanelUser(t, h, "outsider")
	if err := s.UpdateUser("admin", "outsider", "user", false, ""); err != nil {
		t.Fatal(err)
	}
	if got := c.call("GET", "/projects/members/suggestions?project=demo&q=alice", nil).Code; got != 303 {
		t.Fatalf("disabled session = %d", got)
	}
}

func TestMemberSuggestionsLimit(t *testing.T) {
	s, h := memberPanelFixture(t)
	for i := 0; i < 12; i++ {
		if err := s.CreateUser("admin", fmt.Sprintf("candidate%02d", i), testPassword, "user"); err != nil {
			t.Fatal(err)
		}
	}
	c := loginPanelUser(t, h, "owner")
	w := c.call("GET", "/projects/members/suggestions?project=demo&q=candidate", nil)
	var result struct {
		Usernames []string `json:"usernames"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Usernames) != 10 || result.Usernames[0] != "candidate00" || result.Usernames[9] != "candidate09" {
		t.Fatalf("unbounded/unstable results: %+v", result)
	}
}

func TestMemberActionsAndModalErrors(t *testing.T) {
	s, h := memberPanelFixture(t)
	// Another active Owner makes a mistaken duplicate-add downgrade possible,
	// so this case checks the add-only guard rather than the last-Owner guard.
	if err := s.SetProjectMember("owner", "demo", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	c := loginPanelUser(t, h, "owner")
	post := func(action, username, role string) *httptest.ResponseRecorder {
		return c.call("POST", "/projects/members", url.Values{"project": {"demo"}, "action": {action}, "username": {username}, "role": {role}})
	}
	w := post("add", "owner", "viewer")
	if w.Code != 400 || !strings.Contains(w.Body.String(), `data-dialog-reopen="add-project-member"`) || !strings.Contains(w.Body.String(), `value="owner"`) || !strings.Contains(w.Body.String(), `value="viewer" selected`) {
		t.Fatalf("duplicate add did not reopen/preserve: %d", w.Code)
	}
	if s.ProjectRole("owner", "demo") != "owner" {
		t.Fatal("add downgraded owner")
	}
	if err := s.SetProjectMember("owner", "demo", "alice", ""); err != nil {
		t.Fatal(err)
	}
	if w := post("add", "alice-off", "viewer"); w.Code != 400 {
		t.Fatalf("disabled add = %d", w.Code)
	}
	if w := post("add", "alice", "developer"); w.Code != 303 || s.ProjectRole("alice", "demo") != "developer" {
		t.Fatalf("add = %d", w.Code)
	}
	if w := post("update", "alice", "viewer"); w.Code != 303 || s.ProjectRole("alice", "demo") != "viewer" {
		t.Fatalf("update = %d", w.Code)
	}
	if w := post("update", "alice", ""); w.Code != 400 || s.ProjectRole("alice", "demo") != "viewer" {
		t.Fatal("role dropdown removed member")
	}
	if w := post("remove", "alice", "owner"); w.Code != 303 || s.ProjectRole("alice", "demo") != "" {
		t.Fatalf("remove = %d", w.Code)
	}
	for _, action := range []string{"update", "remove"} {
		if w := post(action, "owner", "viewer"); w.Code != 400 || s.ProjectRole("owner", "demo") != "owner" {
			t.Fatalf("last owner %s = %d", action, w.Code)
		}
	}
	c.csrf = "wrong"
	if w := post("add", "alice", "owner"); w.Code != 403 || s.ProjectRole("alice", "demo") != "" {
		t.Fatal("missing CSRF enforcement")
	}
	c = loginPanelUser(t, h, "dev")
	if w := post("add", "alice", "owner"); w.Code != 403 {
		t.Fatal("developer mutated members")
	}
}

func TestMemberSelfRemovalReturnsToAccessiblePage(t *testing.T) {
	s, h := memberPanelFixture(t)
	if err := s.SetProjectMember("owner", "demo", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	c := loginPanelUser(t, h, "owner")
	w := c.call("POST", "/projects/members", url.Values{"project": {"demo"}, "action": {"remove"}, "username": {"owner"}})
	if w.Code != 303 || w.Header().Get("Location") != "/" || s.ProjectRole("owner", "demo") != "" {
		t.Fatalf("self removal = %d redirect %s", w.Code, w.Header().Get("Location"))
	}
}
