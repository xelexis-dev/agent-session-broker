package controlpanel

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

type pausedPolicyBackend struct {
	entered chan struct{}
	release chan struct{}
}

func (b *pausedPolicyBackend) Sessions(context.Context, string) ([]Session, error) { return nil, nil }
func (b *pausedPolicyBackend) Detach(context.Context, string, string) error        { return nil }
func (b *pausedPolicyBackend) SessionSuspended(context.Context, string, string) (bool, error) {
	close(b.entered)
	<-b.release
	return false, nil
}

func TestInstanceHTTPPolicyDefaultsPersistsAndCancelsOnlyAfterCommit(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateUser("admin", "member", testPassword, "user"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"demo", "other"} {
		if err = s.CreateProject(name); err != nil {
			t.Fatal(err)
		}
	}
	if s.InstanceHTTPPolicy() || s.AllowHTTP("demo") {
		t.Fatal("new instance allows HTTP")
	}
	streams := map[string]<-chan struct{}{}
	for _, name := range []string{"demo", "other"} {
		raw, err := s.IssueToken(name, "agent")
		if err != nil {
			t.Fatal(err)
		}
		_, changes, ok := s.authorize(name, raw)
		if !ok {
			t.Fatal("fixture authorization")
		}
		streams[name] = changes
	}
	closed := func(name string) bool {
		select {
		case <-streams[name]:
			return true
		default:
			return false
		}
	}
	if err = s.SetInstanceHTTPPolicy("member", true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin changed policy: %v", err)
	}
	write := s.write
	s.write = func([]byte) error { return errors.New("disk unavailable") }
	if err = s.SetInstanceHTTPPolicy("admin", true); err == nil {
		t.Fatal("storage failure accepted")
	}
	if s.InstanceHTTPPolicy() || s.AllowHTTP("demo") || closed("demo") || closed("other") {
		t.Fatal("failed policy published or cancelled streams")
	}
	s.write = write
	if err = s.SetInstanceHTTPPolicy("admin", true); err != nil {
		t.Fatal(err)
	}
	if !s.AllowHTTP("demo") || !s.AllowHTTP("other") || !closed("demo") || !closed("other") {
		t.Fatal("successful policy did not apply to every project stream")
	}
	if allow, ok := s.TransportPolicy("missing"); allow || ok {
		t.Fatal("unknown project reports policy")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.InstanceHTTPPolicy() || !s.AllowHTTP("demo") {
		t.Fatal("HTTP policy lost on restart")
	}
}

func TestInstanceHTTPPolicyMigratesLegacyProjectOptIn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags map[string]bool
		want  bool
	}{
		{"all-enabled", map[string]bool{"a": true, "b": true}, true},
		{"mixed", map[string]bool{"a": true, "b": false}, false},
		{"none", map[string]bool{"a": false, "b": false}, false},
	} {
		for _, storage := range []string{"json-import", "sqlite"} {
			t.Run(tc.name+"/"+storage, func(t *testing.T) {
				dir := t.TempDir()
				seed, err := OpenSQLite(dir)
				if err != nil {
					t.Fatal(err)
				}
				if err = seed.Setup(testPassword); err != nil {
					t.Fatal(err)
				}
				for name := range tc.flags {
					if err = seed.CreateProject(name); err != nil {
						t.Fatal(err)
					}
				}
				seed.mu.Lock()
				legacy := seed.data
				legacy.Projects = maps.Clone(seed.data.Projects)
				for name, allow := range tc.flags {
					p := legacy.Projects[name]
					p.AllowHTTP = allow
					legacy.Projects[name] = p
				}
				raw, err := json.Marshal(legacy)
				seed.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				if err = seed.Close(); err != nil {
					t.Fatal(err)
				}
				dir = t.TempDir() // Import into a fresh SQLite store.
				if err = os.WriteFile(filepath.Join(dir, "control.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
				opener := OpenSQLite
				if storage == "sqlite" {
					db, err := sqlitestore.Open(dir, "control.db")
					if err != nil {
						t.Fatal(err)
					}
					if err = db.Initialize(map[string][]byte{"state": raw}); err != nil {
						t.Fatal(err)
					}
					if err = db.Close(); err != nil {
						t.Fatal(err)
					}
				}
				for range 2 {
					s, err := opener(dir)
					if err != nil {
						t.Fatal(err)
					}
					if s.InstanceHTTPPolicy() != tc.want {
						t.Fatalf("instance policy = %v, want %v", s.InstanceHTTPPolicy(), tc.want)
					}
					for name := range tc.flags {
						allow, ok := s.TransportPolicy(name)
						if !ok || allow != tc.want {
							t.Fatalf("%s policy = %v/%v", name, allow, ok)
						}
						s.mu.RLock()
						cleared := !s.data.Projects[name].AllowHTTP
						s.mu.RUnlock()
						if !cleared {
							t.Fatalf("%s keeps its project opt-in", name)
						}
					}
					if err = s.Close(); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestInstanceHTTPPolicyHTTPAPIAndAdminForm(t *testing.T) {
	s := userStore(t)
	h := New(s, &panelBackend{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	policy := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	w := policy("/oauth/transport-policy?project=demo")
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "token") {
		t.Fatalf("public default policy = %d %q", w.Code, w.Body.String())
	}
	var body struct {
		Project   string `json:"project"`
		AllowHTTP bool   `json:"allow_http"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Project != "demo" || body.AllowHTTP {
		t.Fatalf("default response = %#v, %v", body, err)
	}
	for _, path := range []string{"/oauth/transport-policy", "/oauth/transport-policy?project=missing", "/oauth/transport-policy?project=demo&extra=1", "/oauth/transport-policy?project=demo&project=demo"} {
		if w = policy(path); w.Code != http.StatusNotFound {
			t.Fatalf("%s = %d", path, w.Code)
		}
	}
	for _, actor := range []string{"owner", "developer", "viewer"} {
		c := loginPanelUser(t, h, actor)
		if w = c.call(http.MethodPost, "/admin/http-policy", url.Values{"allowHTTP": {"true"}}); w.Code != http.StatusForbidden {
			t.Fatalf("%s changed policy: %d", actor, w.Code)
		}
		if w = c.call(http.MethodGet, "/admin/settings", nil); w.Code != http.StatusForbidden {
			t.Fatalf("%s opened instance settings: %d", actor, w.Code)
		}
	}
	admin := loginPanelUser(t, h, "admin")
	req := httptest.NewRequest(http.MethodPost, "/admin/http-policy", strings.NewReader("allowHTTP=true"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(admin.cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF = %d", w.Code)
	}
	if w = admin.call(http.MethodPost, "/admin/http-policy", url.Values{"allowHTTP": {"true"}, "project": {"demo"}}); w.Code != http.StatusBadRequest {
		t.Fatalf("extra field accepted = %d", w.Code)
	}
	if w = admin.call(http.MethodPost, "/admin/http-policy", url.Values{"allowHTTP": {"true"}}); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin/settings" {
		t.Fatalf("admin enable = %d %s", w.Code, w.Header().Get("Location"))
	}
	if w = policy("/oauth/transport-policy?project=demo"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"allow_http":true`) {
		t.Fatalf("enabled policy response = %d %q", w.Code, w.Body.String())
	}
	page := admin.call(http.MethodGet, "/admin/settings", nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `action="/admin/http-policy"`) || !strings.Contains(page.Body.String(), `<input type="hidden" name="allowHTTP" value="false"><button aria-label="HTTP 연결 차단">차단</button>`) || !strings.Contains(page.Body.String(), `<span class="badge enabled">허용</span>`) {
		t.Fatalf("admin policy form missing: %d", page.Code)
	}
	settings := admin.call(http.MethodGet, "/projects/settings?project=demo", nil)
	if strings.Contains(settings.Body.String(), "/projects/http-policy") || strings.Contains(settings.Body.String(), "HTTP 연결 정책") {
		t.Fatal("project settings still carries the HTTP policy form")
	}
	if w = admin.call(http.MethodPost, "/projects/http-policy", url.Values{"project": {"demo"}, "allowHTTP": {"true"}}); w.Code != http.StatusNotFound {
		t.Fatalf("removed project policy route = %d", w.Code)
	}
	if w = admin.call(http.MethodPost, "/admin/http-policy", url.Values{"allowHTTP": {"false"}}); w.Code != http.StatusSeeOther || s.InstanceHTTPPolicy() {
		t.Fatalf("inverse action did not disable: %d", w.Code)
	}
	blocked := admin.call(http.MethodGet, "/admin/settings", nil).Body.String()
	if !strings.Contains(blocked, `<input type="hidden" name="allowHTTP" value="true"><button aria-label="HTTP 연결 허용">허용</button>`) || !strings.Contains(blocked, `<span class="badge ">차단</span>`) {
		t.Fatal("blocked policy inverse action or badge missing")
	}
}

func TestMarkedHTTPRequestsAreDeniedByProjectPolicy(t *testing.T) {
	s := userStore(t)
	raw, err := s.IssueProjectTokenFor("owner", "demo", "test")
	if err != nil {
		t.Fatal(err)
	}
	h := New(s, &panelBackend{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, path := range []string{"/mcp?project=demo", "/oauth/device_authorization?project=demo"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("client_id=test&harness=codex&session_id=s&asb_client_instance_id=i&request_id=r"))
		req.Header.Set("Authorization", "Bearer "+raw)
		req.Header.Set("X-ASB-Client-Transport", "http")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "http_not_allowed") {
			t.Fatalf("%s = %d %q", path, w.Code, w.Body.String())
		}
	}
}

func TestPolicyGenerationChangeIsRetryableWhenCredentialRemainsValid(t *testing.T) {
	for _, marked := range []bool{true, false} {
		t.Run(map[bool]string{true: "marked HTTP", false: "unmarked compatibility"}[marked], func(t *testing.T) {
			s := userStore(t)
			raw, err := s.IssueTokenFor("owner", "demo", "test", "owner")
			if err != nil {
				t.Fatal(err)
			}
			if err = s.SetInstanceHTTPPolicy("admin", true); err != nil {
				t.Fatal(err)
			}
			backend := &pausedPolicyBackend{entered: make(chan struct{}), release: make(chan struct{})}
			h := New(s, backend, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				req := httptest.NewRequest(http.MethodPost, "/mcp?project=demo", nil)
				req.Header.Set("Authorization", "Bearer "+raw)
				if marked {
					req.Header.Set("X-ASB-Client-Transport", "http")
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				result <- w
			}()
			<-backend.entered
			if err = s.SetInstanceHTTPPolicy("admin", false); err != nil {
				t.Fatal(err)
			}
			close(backend.release)
			if w := <-result; w.Code != http.StatusServiceUnavailable {
				t.Fatalf("valid credential policy change = %d", w.Code)
			}
		})
	}
}

func TestRevokedCredentialGenerationChangeRemainsUnauthorized(t *testing.T) {
	s := userStore(t)
	raw, err := s.IssueTokenFor("owner", "demo", "test", "owner")
	if err != nil {
		t.Fatal(err)
	}
	backend := &pausedPolicyBackend{entered: make(chan struct{}), release: make(chan struct{})}
	h := New(s, backend, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "https://broker/mcp?project=demo", nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		result <- w
	}()
	<-backend.entered
	if err = s.RevokeTokenFor("owner", "demo", s.ProjectsFor("owner")[0].Tokens[0].ID); err != nil {
		t.Fatal(err)
	}
	close(backend.release)
	if w := <-result; w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked credential policy change = %d", w.Code)
	}
}
