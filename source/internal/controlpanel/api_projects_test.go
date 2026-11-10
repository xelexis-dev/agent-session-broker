package controlpanel

import (
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestAPIProjectCredentialLifecycle(t *testing.T) {
	_, h := apiFixture(t)
	w := apiRequest(h, "POST", "/api/v1/login", `{"username":"admin","password":"`+testPassword+`"}`, "")
	if w.Code != 201 {
		t.Fatalf("login=%d", w.Code)
	}
	var login apiLoginResponse
	if json.Unmarshal(w.Body.Bytes(), &login) != nil {
		t.Fatal("login JSON")
	}
	w = apiRequest(h, "POST", "/api/v1/projects", `{"name":"synthetic"}`, login.Token)
	if w.Code != 201 {
		t.Fatalf("create=%d want 201", w.Code)
	}
	w = apiRequest(h, "POST", "/api/v1/projects/synthetic/tokens", `{"label":"fixture"}`, login.Token)
	if w.Code != 201 {
		t.Fatalf("issue=%d", w.Code)
	}
	var issued struct {
		Token string `json:"token"`
		ID    string `json:"id"`
	}
	if json.Unmarshal(w.Body.Bytes(), &issued) != nil || issued.Token == "" {
		t.Fatal("issuance JSON")
	}
	device := func() int {
		r := httptest.NewRequest("POST", "https://example.test/oauth/device_authorization?project=synthetic", strings.NewReader(`client_id=asb-client-v1&harness=codex&session_id=synthetic-conversation&asb_client_instance_id=synthetic-instance&request_id=synthetic-request`))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Authorization", "Bearer "+issued.Token)
		v := httptest.NewRecorder()
		h.ServeHTTP(v, r)
		return v.Code
	}
	if got := device(); got != http.StatusOK {
		t.Fatalf("device before revoke=%d", got)
	}
	if w = apiRequest(h, "DELETE", "/api/v1/projects/synthetic/tokens/"+issued.ID, "", login.Token); w.Code != 204 {
		t.Fatalf("revoke=%d", w.Code)
	}
	if got := device(); got != 401 {
		t.Fatalf("device after revoke=%d", got)
	}
	if w = apiRequest(h, "POST", "/api/v1/logout", "", login.Token); w.Code != 204 {
		t.Fatalf("logout=%d", w.Code)
	}
}

func TestAPIProjectGenerationRetry(t *testing.T) {
	s, h, _, raw, _ := projectParityFixture(t, "admin")
	g := s.ProjectGeneration("demo")
	if w := apiRequest(h, "DELETE", "/api/v1/projects/demo?confirm=demo&generation="+g, "", raw); w.Code != 204 {
		t.Fatalf("delete=%d", w.Code)
	}
	if w := apiRequest(h, "POST", "/api/v1/projects", `{"name":"demo"}`, raw); w.Code != 201 {
		t.Fatalf("recreate=%d", w.Code)
	}
	if w := apiRequest(h, "POST", "/api/v1/projects/demo/tokens", `{}`, raw); w.Code != 201 {
		t.Fatalf("new token=%d", w.Code)
	}
	before := normalizedProjectState(t, s)
	w := apiRequest(h, "DELETE", "/api/v1/projects/demo?confirm=demo&generation="+g, "", raw)
	if w.Code != 409 {
		t.Fatalf("stale delete=%d", w.Code)
	}
	if !reflect.DeepEqual(before, normalizedProjectState(t, s)) {
		t.Fatal("stale delete mutated generation")
	}
	if w = apiRequest(h, "PATCH", "/api/v1/projects/demo", `{"enabled":false,"generation":"`+g+`"}`, raw); w.Code != 409 {
		t.Fatal("stale PATCH admitted")
	}
}
func TestAPIProjectWriteBarriers(t *testing.T) {
	for _, operation := range []string{"create", "issue"} {
		for _, change := range []string{"revoke", "password", "off"} {
			t.Run(operation+"/"+change, func(t *testing.T) {
				s, h := apiFixture(t)
				if err := s.CreateProjectFor("admin", "demo"); err != nil {
					t.Fatal(err)
				}
				raw, _ := apiIssue(t, s)
				path, body := "/api/v1/projects", `{"name":"new"}`
				if operation == "issue" {
					path = "/api/v1/projects/demo/tokens"
					body = `{}`
				}
				entered, release := make(chan struct{}), make(chan struct{})
				reader := &policyReader{Reader: strings.NewReader(body), change: func() { close(entered); <-release }}
				r := httptest.NewRequest("POST", "https://example.test"+path, reader)
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer "+raw)
				w := httptest.NewRecorder()
				done := make(chan struct{})
				go func() { h.ServeHTTP(w, r); close(done) }()
				<-entered
				switch change {
				case "revoke":
					if err := s.RevokeOwnUserTokens("admin"); err != nil {
						t.Fatal(err)
					}
				case "password":
					if err := s.ChangeOwnPassword("admin", testPassword, "another synthetic password", "another synthetic password"); err != nil {
						t.Fatal(err)
					}
				case "off":
					if err := s.SetAdminAPI("admin", false); err != nil {
						t.Fatal(err)
					}
				}
				close(release)
				<-done
				if w.Code != 401 && w.Code != 403 {
					t.Fatalf("stale write=%d", w.Code)
				}
				if _, exists := s.data.Projects["new"]; exists {
					t.Fatal("stale create committed")
				}
				if len(s.data.Projects["demo"].Tokens) != 0 {
					t.Fatal("stale issuance committed")
				}
			})
		}
	}
}
func TestAPIProjectTokenOnceVersionAndAudit(t *testing.T) {
	s, h, _, raw, _ := projectParityFixture(t, "admin")
	w := apiRequest(h, "POST", "/api/v1/projects/demo/tokens", `{"label":"one-time"}`, raw)
	if w.Code != 201 {
		t.Fatalf("issue=%d", w.Code)
	}
	var issued apiProjectTokenResponse
	if json.Unmarshal(w.Body.Bytes(), &issued) != nil || issued.Version == "" {
		t.Fatal("version missing")
	}
	for _, path := range []string{"/api/v1/projects", "/api/v1/projects/demo", "/api/v1/projects/demo/tokens", "/api/v1/projects/demo/members"} {
		w = apiRequest(h, "GET", path, "", raw)
		if w.Code != 200 || strings.Contains(w.Body.String(), issued.Token) || strings.Contains(w.Header().Get("Location"), issued.Token) {
			t.Fatal("raw token repeated")
		}
	}
	// Use the catalog constants rather than coupling the test to display spelling.
	scopes, _ := json.Marshal([]string{access.SessionRegister, access.MessageReceive})
	w = apiRequest(h, "PUT", "/api/v1/projects/demo/tokens/"+issued.ID+"/scopes", `{"scopes":`+string(scopes)+`,"version":"`+issued.Version+`"}`, raw)
	if w.Code != 200 {
		t.Fatalf("scope edit=%d", w.Code)
	}
	w = apiRequest(h, "PUT", "/api/v1/projects/demo/tokens/"+issued.ID+"/scopes", `{"scopes":`+string(scopes)+`,"version":"`+issued.Version+`"}`, raw)
	if w.Code != 409 {
		t.Fatal("stale scope version admitted")
	}
	w = apiRequest(h, "PATCH", "/api/v1/projects/demo/members/viewer", `{"role":"developer"}`, raw)
	if w.Code != 200 {
		t.Fatal("member write")
	}
	found := false
	for _, event := range s.api.audits {
		if event.Action == "PATCH /api/v1/projects/{project}/members/{username}" && event.Target == "viewer" && event.Role == "developer" {
			found = true
		}
	}
	if !found {
		t.Fatal("member audit lacks target/role")
	}
}
func TestAPIProjectLastOwnerAndDTO(t *testing.T) {
	for _, actor := range []string{"viewer", "developer-own"} {
		t.Run(actor, func(t *testing.T) {
			s, h, _, raw, _ := projectParityFixture(t, actor)
			s.mu.Lock()
			p := s.data.Projects["demo"]
			p.AutoBindings = []AutoBinding{{Identity: "private-bound-identity"}}
			p.AutoExchanges = []AutoExchange{{RequestID: "private-request"}}
			for i := range p.Tokens {
				p.Tokens[i].BoundIdentity = "private-token-identity"
			}
			s.data.Projects["demo"] = p
			s.mu.Unlock()
			for _, path := range []string{"/api/v1/projects", "/api/v1/projects/demo", "/api/v1/projects/demo/tokens"} {
				w := apiRequest(h, "GET", path, "", raw)
				if w.Code != 200 {
					t.Fatal("projection read")
				}
				for _, bad := range []string{"private-", "autoBindings", "autoExchanges", "boundIdentity", "tokenHash", "requestID"} {
					if strings.Contains(w.Body.String(), bad) {
						t.Fatal("internal field disclosed")
					}
				}
			}
		})
	}
	s, h, _, raw, _ := projectParityFixture(t, "owner")
	before := normalizedProjectState(t, s)
	w := apiRequest(h, "DELETE", "/api/v1/projects/demo/members/owner", "", raw)
	if w.Code != 400 {
		t.Fatalf("last owner=%d", w.Code)
	}
	if !reflect.DeepEqual(before, normalizedProjectState(t, s)) {
		t.Fatal("last owner removed")
	}
}
