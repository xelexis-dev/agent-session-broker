package controlpanel

import (
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

type projectAPICase struct {
	parity, method, path, body, formPath string
	form                                 url.Values
}

func projectCases(s *Store, token string) []projectAPICase {
	g := s.ProjectGeneration("demo")
	version := ""
	for _, t := range s.data.Projects["demo"].Tokens {
		if t.ID == token {
			version = tokenScopeVersion("demo", g, t)
		}
	}
	scopes, _ := json.Marshal([]string{access.SessionRegister, access.MessageReceive})
	return []projectAPICase{
		{"project-list", "GET", "/api/v1/projects", "", "/", nil},
		{"project-create", "POST", "/api/v1/projects", `{"name":"new-project"}`, "/projects/create", url.Values{"name": {"new-project"}}},
		{"project-get", "GET", "/api/v1/projects/demo", "", "/projects/settings?project=demo", nil},
		{"project-patch", "PATCH", "/api/v1/projects/demo", `{"enabled":false}`, "/projects/save", url.Values{"name": {"demo"}, "mode": {"token"}}},
		{"project-delete", "DELETE", "/api/v1/projects/demo?confirm=demo&generation=" + g, "", "/projects/delete", url.Values{"project": {"demo"}, "confirmName": {"demo"}}},
		{"member-list", "GET", "/api/v1/projects/demo/members", "", "/projects/settings?project=demo", nil},
		{"member-add", "POST", "/api/v1/projects/demo/members", `{"username":"outsider","role":"viewer"}`, "/projects/members", url.Values{"project": {"demo"}, "username": {"outsider"}, "role": {"viewer"}, "action": {"add"}}},
		{"member-update", "PATCH", "/api/v1/projects/demo/members/viewer", `{"role":"developer"}`, "/projects/members", url.Values{"project": {"demo"}, "username": {"viewer"}, "role": {"developer"}, "action": {"update"}}},
		{"member-remove", "DELETE", "/api/v1/projects/demo/members/viewer", "", "/projects/members", url.Values{"project": {"demo"}, "username": {"viewer"}, "action": {"remove"}}},
		{"project-token-list", "GET", "/api/v1/projects/demo/tokens", "", "/projects/settings?project=demo", nil},
		{"project-token-issue", "POST", "/api/v1/projects/demo/tokens", `{"label":"new-token"}`, "/tokens/project", url.Values{"project": {"demo"}, "label": {"new-token"}}},
		{"project-token-revoke", "DELETE", "/api/v1/projects/demo/tokens/" + token, "", "/tokens/revoke", url.Values{"project": {"demo"}, "id": {token}}},
		{"project-token-scopes", "PUT", "/api/v1/projects/demo/tokens/" + token + "/scopes", `{"scopes":` + string(scopes) + `,"version":"` + version + `"}`, "/tokens/scopes", url.Values{"project": {"demo"}, "id": {token}, "scope": {access.SessionRegister, access.MessageReceive}, "intent": {"apply"}}},
		{"token-catalog", "GET", "/api/v1/token-scope-catalog", "", "/", nil},
	}
}
func projectParityFixture(t *testing.T, actorCase string) (*Store, http.Handler, *userPanelClient, string, string) {
	t.Helper()
	s := userStore(t)
	if err := s.CreateUser("admin", "disabled", testPassword, "user"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectMember("owner", "demo", "disabled", "developer"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAdminAPI("admin", true); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Date(2026, 11, 5, 11, 0, 0, 0, time.UTC) }
	for _, owner := range []string{"owner", "developer"} {
		if _, err := s.IssueProjectTokenFor(owner, "demo", owner+"-token"); err != nil {
			t.Fatal(err)
		}
	}
	actor := actorCase
	if strings.HasPrefix(actor, "developer-") {
		actor = "developer"
	}
	if actor == "nonmember" {
		actor = "outsider"
	}
	h := New(s, &misuseBackend{rows: map[string][]Session{}}, http.NotFoundHandler())
	client := loginPanelUser(t, h, actor)
	actorUser, _ := s.User(actor)
	raw, _, err := s.issueUserToken(actor, testPassword, "", 0, []string{"read", "write"}, "panel", "synthetic-panel", false, actorUser.Revision)
	if err != nil {
		t.Fatal(err)
	}
	targetOwner := "developer"
	if actorCase == "developer-other" {
		targetOwner = "owner"
	}
	target := ""
	for _, token := range s.data.Projects["demo"].Tokens {
		if token.Kind == tokenProject && token.OwnerID == targetOwner {
			target = token.ID
		}
	}
	if actor == "disabled" {
		if err := s.UpdateUser("admin", actor, "user", false, ""); err != nil {
			t.Fatal(err)
		}
	}
	return s, h, client, raw, target
}
func normalizedProjectState(t *testing.T, s *Store) any {
	t.Helper()
	s.mu.RLock()
	raw, _ := json.Marshal(s.data)
	s.mu.RUnlock()
	var state any
	_ = json.Unmarshal(raw, &state)
	var clean func(any)
	clean = func(v any) {
		switch object := v.(type) {
		case map[string]any:
			for key, value := range object {
				if key == "salt" || key == "hash" || key == "tokenHash" || key == "id" || key == "projectGenerationId" || strings.HasSuffix(key, "At") {
					delete(object, key)
				} else {
					clean(value)
				}
			}
		case []any:
			for _, item := range object {
				clean(item)
			}
		}
	}
	clean(state)
	return state
}
func TestAPIProjectFormParityMatrix(t *testing.T) {
	covered := map[string]bool{}
	for _, actor := range []string{"admin", "owner", "developer-own", "developer-other", "viewer", "nonmember", "disabled"} {
		for index, definition := range projectCasesForCoverage() {
			t.Run(actor+"/"+definition, func(t *testing.T) {
				formStore, _, browser, _, formID := projectParityFixture(t, actor)
				apiStore, handler, _, raw, apiID := projectParityFixture(t, actor)
				formCase := projectCases(formStore, formID)[index]
				apiCase := projectCases(apiStore, apiID)[index]
				covered[apiCase.parity] = true
				if apiCase.parity == "project-token-scopes" {
					p := formStore.data.Projects["demo"]
					for _, token := range p.Tokens {
						if token.ID == formID {
							formCase.form.Set("settings_version", tokenScopeVersion("demo", p.ProjectGenerationID, token))
						}
					}
				}
				method := "POST"
				if formCase.method == "GET" {
					method = "GET"
				}
				form := browser.call(method, formCase.formPath, formCase.form)
				api := apiRequest(handler, apiCase.method, apiCase.path, apiCase.body, raw)
				formAllowed := form.Code < 400 && !strings.HasPrefix(form.Header().Get("Location"), "/login")
				apiAllowed := api.Code < 400
				if formAllowed != apiAllowed {
					t.Fatalf("form=%d API=%d", form.Code, api.Code)
				}
				if !reflect.DeepEqual(normalizedProjectState(t, formStore), normalizedProjectState(t, apiStore)) {
					t.Fatal("normalized state differs")
				}
			})
		}
	}
	for _, route := range apiRoutes {
		if strings.HasPrefix(route.Parity, "project-") || strings.HasPrefix(route.Parity, "member-") || route.Parity == "token-catalog" {
			if !covered[route.Parity] {
				t.Fatalf("missing parity coverage %s", route.Parity)
			}
		}
	}
}
func projectCasesForCoverage() []string {
	return []string{"project-list", "project-create", "project-get", "project-patch", "project-delete", "member-list", "member-add", "member-update", "member-remove", "project-token-list", "project-token-issue", "project-token-revoke", "project-token-scopes", "token-catalog"}
}

func TestAPIProjectScopeSweep(t *testing.T) {
	for _, parity := range projectCasesForCoverage() {
		t.Run(parity, func(t *testing.T) {
			s, h, _, _, id := projectParityFixture(t, "admin")
			var tc projectAPICase
			for _, item := range projectCases(s, id) {
				if item.parity == parity {
					tc = item
				}
			}
			scope := "read"
			if tc.method == "GET" {
				scope = "write"
			}
			actorUser, _ := s.User("admin")
			raw, _, err := s.issueUserToken("admin", testPassword, "", 0, []string{scope}, "panel", "scope-sweep", false, actorUser.Revision)
			if err != nil {
				t.Fatal(err)
			}
			before := normalizedProjectState(t, s)
			w := apiRequest(h, tc.method, tc.path, tc.body, raw)
			if w.Code != 403 {
				t.Fatalf("narrow scope=%d", w.Code)
			}
			if !reflect.DeepEqual(before, normalizedProjectState(t, s)) {
				t.Fatal("narrow scope mutated state")
			}
		})
	}
}

func TestAPIProjectRouteSmoke(t *testing.T) {
	for _, parity := range projectCasesForCoverage() {
		t.Run(parity, func(t *testing.T) {
			s, h, _, raw, id := projectParityFixture(t, "admin")
			var tc projectAPICase
			for _, item := range projectCases(s, id) {
				if item.parity == parity {
					tc = item
				}
			}
			var route apiRoute
			for _, v := range apiRoutes {
				if v.Parity == parity {
					route = v
				}
			}
			w := apiRequest(h, tc.method, tc.path, tc.body, raw)
			if w.Code != route.Status {
				t.Fatalf("status=%d want %d", w.Code, route.Status)
			}
			if route.Status == 201 && w.Header().Get("Location") == "" {
				t.Fatal("creation Location missing")
			}
			if route.Response != nil {
				var object map[string]json.RawMessage
				if json.Unmarshal(w.Body.Bytes(), &object) != nil {
					t.Fatal("response JSON")
				}
				var keys func(reflect.Type) []string
				keys = func(typ reflect.Type) []string {
					out := []string{}
					for i := 0; i < typ.NumField(); i++ {
						field := typ.Field(i)
						if field.Anonymous {
							out = append(out, keys(field.Type)...)
						} else {
							out = append(out, strings.Split(field.Tag.Get("json"), ",")[0])
						}
					}
					return out
				}
				want := keys(route.Response)
				if len(object) != len(want) {
					t.Fatal("response keys")
				}
				for _, key := range want {
					if _, ok := object[key]; !ok {
						t.Fatal("missing response key")
					}
				}
			}
		})
	}
}
