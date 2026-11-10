package controlpanel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAPIAdminUserReauthenticationLifecycle(t *testing.T) {
	s, h := apiFixture(t)
	raw, _ := apiIssue(t, s, "admin", "read", "write", "approve")
	body := func(fields map[string]any) string {
		b, e := json.Marshal(fields)
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	create := map[string]any{"username": "managed", "password": testPassword, "role": "user"}
	if w := apiRequest(h, "POST", "/api/v1/admin/users", body(create), raw); w.Code != 403 {
		t.Fatalf("create without reauth status=%d want403", w.Code)
	}
	create["current_password"] = testPassword
	if w := apiRequest(h, "POST", "/api/v1/admin/users", body(create), raw); w.Code != 201 {
		t.Fatalf("create with reauth status=%d want201", w.Code)
	}
	if w := apiRequest(h, "PATCH", "/api/v1/admin/users/managed", `{"role":"admin"}`, raw); w.Code != 403 {
		t.Fatalf("promotion without reauth status=%d", w.Code)
	}
	if w := apiRequest(h, "PATCH", "/api/v1/admin/users/managed", body(map[string]any{"role": "admin", "current_password": testPassword}), raw); w.Code != 200 {
		t.Fatalf("promotion status=%d", w.Code)
	}
	if w := apiRequest(h, "PATCH", "/api/v1/admin/users/managed", `{"role":"user"}`, raw); w.Code != 200 {
		t.Fatalf("demotion status=%d", w.Code)
	}
}

type apiAdminBackend struct {
	fail                string
	reads               int
	mu                  sync.Mutex
	sessions            []Session
	detached, suspended int
	entered, release    chan struct{}
	once                sync.Once
}

func (b *apiAdminBackend) Sessions(ctx context.Context, _ string) ([]Session, error) {
	b.mu.Lock()
	b.reads++
	if b.fail == "sessions" {
		b.mu.Unlock()
		return nil, ErrStorage
	}
	b.mu.Unlock()
	if b.entered != nil {
		b.once.Do(func() { close(b.entered) })
		select {
		case <-b.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Session(nil), b.sessions...), nil
}
func (b *apiAdminBackend) Detach(context.Context, string, string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail == "detach" {
		return ErrStorage
	}
	b.detached++
	return nil
}
func (b *apiAdminBackend) SetSuspended(_ context.Context, _, id string, suspended bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail == "suspend" {
		return ErrStorage
	}
	b.suspended++
	for i := range b.sessions {
		if b.sessions[i].ID == id {
			b.sessions[i].Suspended = suspended
		}
	}
	return nil
}

type adminRouteFixture struct {
	s      *Store
	h      http.Handler
	b      *apiAdminBackend
	device DeviceResponse
	grant  string
}

func newAdminRouteFixture(t *testing.T) adminRouteFixture {
	t.Helper()
	s := userStore(t)
	if err := s.CreateUser("admin", "otherdev", testPassword, "user"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectMember("owner", "demo", "otherdev", "developer"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAdminAPI("admin", true); err != nil {
		t.Fatal(err)
	}
	parent, err := s.IssueProjectTokenFor("developer", "demo", "fixture-parent")
	if err != nil {
		t.Fatal(err)
	}
	device, err := s.BeginDeviceAuthorization("demo", parent, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "pending", InstanceID: "pc", RequestID: "pending-request"})
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := s.BeginDeviceAuthorization("demo", parent, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "reserved", InstanceID: "pc", RequestID: "reserved-request"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApproveDeviceAuthorization("developer", reserved.UserCode, "reserved-agent", false); err != nil {
		t.Fatal(err)
	}
	grant := ""
	for _, token := range s.data.Projects["demo"].Tokens {
		if token.Nickname == "reserved-agent" {
			grant = token.ID
		}
	}
	b := &apiAdminBackend{sessions: []Session{{ID: "backend-session", RegistrationID: "reg-target", OwnerID: "developer", Name: "agent", Harness: "codex", SessionID: "sid", ClientID: "pc", Status: "alive"}}}
	return adminRouteFixture{s, New(s, b, http.NotFoundHandler()), b, device, grant}
}
func issueAdminActor(t *testing.T, s *Store, actor string, scopes ...string) (string, UserTokenView) {
	t.Helper()
	raw, v, err := s.issueUserToken(actor, testPassword, "fixture", 0, scopes, "login", "fixture", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	return raw, v
}
func marshalAdmin(t *testing.T, v any) string {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal("fixture JSON")
	}
	return string(b)
}
func adminRouteInput(t *testing.T, f adminRouteFixture, route apiRoute) (string, string) {
	path := strings.ReplaceAll(route.Path, "{project}", "demo")
	path = strings.ReplaceAll(path, "{registrationId}", "reg-target")
	path = strings.ReplaceAll(path, "{tokenId}", "TOKEN-FIXTURE")
	target := "developer"
	if route.Parity == "admin-api-release" {
		target = f.grant
	}
	path = strings.ReplaceAll(path, "{id}", target)
	body := ""
	switch route.Parity {
	case "admin-api-detach", "admin-api-release":
		body = `{}`
	case "admin-api-suspension":
		body = `{"suspended":true}`
	case "admin-api-device-lookup":
		body = marshalAdmin(t, map[string]any{"user_code": f.device.UserCode})
	case "admin-api-device-decision":
		body = marshalAdmin(t, map[string]any{"user_code": f.device.UserCode, "action": "approve", "nickname": "approved-agent"})
	case "admin-api-admin-audit":
		path += "?kind=api"
	case "admin-api-user-create":
		body = marshalAdmin(t, map[string]any{"username": "created", "password": testPassword, "role": "user", "current_password": testPassword})
	case "admin-api-user-update":
		body = `{"enabled":false}`
	case "admin-api-http-policy":
		body = `{"allow_http":false}`
	case "admin-api-scope-policy", "admin-api-attachment-policy":
		body = `{"enabled":false}`
	}
	return path, body
}
func apiAdminRouteSmoke(t *testing.T, route apiRoute) {
	t.Helper()
	f := newAdminRouteFixture(t)
	raw, v := issueAdminActor(t, f.s, "admin", "admin", "read", "write", "approve")
	path, body := adminRouteInput(t, f, route)
	if route.Parity == "admin-api-user-token-revoke" {
		path = strings.ReplaceAll(path, "TOKEN-FIXTURE", v.ID)
		path = strings.Replace(path, "/users/developer/", "/users/admin/", 1)
	}
	w := apiRequest(f.h, route.Method, path, body, raw)
	if w.Code != route.Status {
		t.Fatalf("route %s status=%d want=%d", route.Parity, w.Code, route.Status)
	}
	if route.Response != nil {
		var object map[string]json.RawMessage
		if json.Unmarshal(w.Body.Bytes(), &object) != nil || len(object) != route.Response.NumField() {
			t.Fatal("route DTO key mismatch")
		}
	}
}

func TestAPIAdminApproveScopeAndSharedCodeLimit(t *testing.T) {
	f := newAdminRouteFixture(t)
	plain, _ := issueAdminActor(t, f.s, "owner", "read", "write")
	approve, _ := issueAdminActor(t, f.s, "owner", "approve")
	body := marshalAdmin(t, map[string]any{"user_code": f.device.UserCode})
	if w := apiRequest(f.h, "POST", "/api/v1/device-approvals/lookup", body, plain); w.Code != 403 {
		t.Fatal("write implies approve")
	}
	client := loginPanelUser(t, f.h, "owner")
	for range 10 {
		if w := client.call("GET", "/auth/code?code="+url.QueryEscape(f.device.UserCode), nil); w.Code != 200 {
			t.Fatal("panel lookup fixture")
		}
	}
	if w := apiRequest(f.h, "POST", "/api/v1/device-approvals/lookup", body, approve); w.Code != 429 {
		t.Fatalf("code limiter is not shared=%d", w.Code)
	}
}
func TestAPIAdminAuditCursorAndScopeSweep(t *testing.T) {
	f := newAdminRouteFixture(t)
	raw, _ := issueAdminActor(t, f.s, "admin", "admin", "read", "write", "approve")
	f.s.mu.Lock()
	base := f.s.now()
	for i := range 5 {
		at := base.Add(time.Duration(i) * time.Second)
		f.s.now = func() time.Time { return at }
		if err := f.s.apiBatchLocked(map[string][]byte{}, nil, apiAuditEvent{Action: fmt.Sprintf("fixture-%d", i), Actor: "admin", Result: "success"}); err != nil {
			t.Fatal(err)
		}
	}
	f.s.mu.Unlock()
	w := apiRequest(f.h, "GET", "/api/v1/admin/audit?kind=api&limit=2", "", raw)
	var first apiAdminAuditPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &first) != nil || len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatal("audit first page")
	}
	path := "/api/v1/admin/audit?kind=api&limit=2&cursor=" + url.QueryEscape(first.NextCursor)
	w = apiRequest(f.h, "GET", path, "", raw)
	var next apiAdminAuditPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &next) != nil || len(next.Items) != 2 {
		t.Fatal("audit next page")
	}
	if reflect.DeepEqual(first.Items, next.Items) {
		t.Fatal("cursor repeated first page")
	}
	for _, query := range []string{"kind=registration&cursor=" + first.NextCursor, "kind=api&cursor=broken", "kind=api&limit=201", "kind=api&limit=1&limit=2"} {
		if w := apiRequest(f.h, "GET", "/api/v1/admin/audit?"+query, "", raw); w.Code != 400 {
			t.Fatal("invalid audit cursor/query admitted")
		}
	}
	read, _ := issueAdminActor(t, f.s, "admin", "read")
	for _, route := range adminAPIRoutes() {
		if route.Method == "GET" && route.Scope == "read" {
			continue
		}
		path, body := adminRouteInput(t, f, route)
		if w := apiRequest(f.h, route.Method, path, body, read); w.Code != 403 {
			t.Fatalf("scope sweep %s=%d", route.Parity, w.Code)
		}
	}
}
func TestAPIAdminCommitBarriers(t *testing.T) {
	for _, op := range []string{"user", "detach"} {
		for _, kind := range []string{"revoke", "switch"} {
			t.Run(op+kind, func(t *testing.T) {
				f := newAdminRouteFixture(t)
				raw, v := issueAdminActor(t, f.s, "admin", "admin", "write")
				entered, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				jsonBody := `{"enabled":false}`
				method, path := "PATCH", "/api/v1/admin/users/developer"
				if op == "detach" {
					method, path, jsonBody = "POST", "/api/v1/projects/demo/sessions/reg-target/detach", `{}`
				}
				body := &policyReader{Reader: strings.NewReader(jsonBody), change: func() { once.Do(func() { close(entered); <-release }) }}
				r := httptest.NewRequest(method, "https://example.test"+path, body)
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer "+raw)
				w := httptest.NewRecorder()
				done := make(chan struct{})
				go func() { f.h.ServeHTTP(w, r); close(done) }()
				<-entered
				if kind == "switch" {
					if err := f.s.SetAdminAPI("admin", false); err != nil {
						t.Fatal(err)
					}
				} else {
					f.s.mu.Lock()
					err := f.s.revokeUserTokensLocked("admin", v.ID, false)
					f.s.mu.Unlock()
					if err != nil {
						t.Fatal(err)
					}
				}
				close(release)
				<-done
				if w.Code != 401 && w.Code != 403 {
					t.Fatalf("stale principal committed=%d", w.Code)
				}
				u, _ := f.s.User("developer")
				if !u.Enabled || f.b.detached != 0 {
					t.Fatal("blocked operation changed target")
				}
			})
		}
	}
}
func TestAPIAdminDemotionFailureBlocksTargetTokens(t *testing.T) {
	f := newAdminRouteFixture(t)
	admin, _ := issueAdminActor(t, f.s, "admin", "admin")
	target, _ := issueAdminActor(t, f.s, "developer", "read")
	f.s.mu.Lock()
	write := f.s.write
	f.s.write = func([]byte) error { return ErrStorage }
	f.s.mu.Unlock()
	w := apiRequest(f.h, "PATCH", "/api/v1/admin/users/developer", `{"enabled":false}`, admin)
	if w.Code != 503 {
		t.Fatalf("storage failure=%d", w.Code)
	}
	if _, err := f.s.authenticateAPI(target); err == nil {
		t.Fatal("failed disable left target API token active")
	}
	f.s.mu.Lock()
	f.s.write = write
	pending := len(f.s.api.pendingDeletes)
	err := f.s.sweepAPILocked()
	f.s.mu.Unlock()
	if pending == 0 || err != nil {
		t.Fatal("failed disable omitted pending token deletion")
	}
	if u, _ := f.s.User("developer"); !u.Enabled {
		t.Fatal("panel user record changed before durable commit")
	}
}
func TestAPIAdminHTTPReauthAndUserTokenOwnership(t *testing.T) {
	f := newAdminRouteFixture(t)
	raw, _ := issueAdminActor(t, f.s, "admin", "admin")
	if w := apiRequest(f.h, "PUT", "/api/v1/admin/settings/http-policy", `{"allow_http":true}`, raw); w.Code != 403 {
		t.Fatal("plaintext enable skipped reauth")
	}
	body := marshalAdmin(t, map[string]any{"allow_http": true, "current_password": testPassword})
	if w := apiRequest(f.h, "PUT", "/api/v1/admin/settings/http-policy", body, raw); w.Code != 204 {
		t.Fatalf("HTTP policy reauth=%d", w.Code)
	}
	if w := apiRequest(f.h, "PUT", "/api/v1/admin/settings/http-policy", `{"allow_http":false}`, raw); w.Code != 204 {
		t.Fatal("HTTP disable required reauth")
	}
	_, view := issueAdminActor(t, f.s, "owner", "read")
	if w := apiRequest(f.h, "DELETE", "/api/v1/admin/users/developer/tokens/"+view.ID, "", raw); w.Code != 404 {
		t.Fatal("cross-user token id admitted")
	}
	if w := apiRequest(f.h, "POST", "/api/v1/admin/users/owner/tokens", `{}`, raw); w.Code != 405 {
		t.Fatal("admin can mint another user's token")
	}
}

func adminParityForm(t *testing.T, f adminRouteFixture, route apiRoute) (string, string, url.Values, bool) {
	t.Helper()
	form := url.Values{"project": {"demo"}}
	method, path := "GET", ""
	switch route.Parity {
	case "admin-api-sessions":
		path = "/projects/sessions?project=demo"
	case "admin-api-detach":
		method, path = "POST", "/sessions/detach"
		form.Set("id", "backend-session")
	case "admin-api-suspension":
		method, path = "POST", "/sessions/suspend"
		form.Set("id", "backend-session")
	case "admin-api-release":
		method, path = "POST", "/sessions/detach"
		form.Set("grant_id", f.grant)
	case "admin-api-device-lookup":
		path = "/auth/code?code=" + url.QueryEscape(f.device.UserCode)
	case "admin-api-device-decision":
		method, path = "POST", "/auth/code"
		form = url.Values{"code": {f.device.UserCode}, "action": {"approve"}, "nickname": {"approved-agent"}}
	case "admin-api-project-audit":
		path = "/projects/audit?project=demo"
	case "admin-api-users":
		path = "/admin/users"
	case "admin-api-user":
		path = "/admin/users/detail?id=developer"
	case "admin-api-user-create":
		method, path = "POST", "/admin/users/create"
		form = url.Values{"username": {"created"}, "password": {testPassword}, "role": {"user"}}
	case "admin-api-user-update":
		method, path = "POST", "/admin/users/update"
		form = url.Values{"id": {"developer"}, "role": {"user"}}
	case "admin-api-settings":
		path = "/admin/settings"
	case "admin-api-http-policy":
		method, path = "POST", "/admin/http-policy"
		form = url.Values{"settings_version": {policySettingsVersion("http-policy", f.s.data.AllowHTTP)}}
	case "admin-api-scope-policy":
		method, path = "POST", "/admin/scope-issuance"
		form = url.Values{"enabled": {"false"}, "settings_version": {policySettingsVersion("scope-issuance", bool(f.s.data.ScopeIssuanceReady))}}
	case "admin-api-attachment-policy":
		method, path = "POST", "/admin/attachments"
		form = url.Values{"attachments": {"false"}, "settings_version": {policySettingsVersion("attachments", f.s.attachmentEnabled)}}
	case "admin-api-admin-audit", "admin-api-user-tokens", "admin-api-user-tokens-revoke", "admin-api-user-token-revoke":
		return "", "", nil, false
	default:
		t.Fatal("new MR-D route lacks parity declaration")
	}
	return method, path, form, true
}
func adminParitySnapshot(f adminRouteFixture) string {
	s := f.s
	s.mu.RLock()
	defer s.mu.RUnlock()
	users := map[string]string{}
	for id, u := range s.data.Users {
		users[id] = fmt.Sprintf("%s/%t/%d", u.Role, u.Enabled, u.Revision)
	}
	tokens, devices := []string{}, []string{}
	for _, token := range s.data.Projects["demo"].Tokens {
		tokens = append(tokens, fmt.Sprintf("%s/%s/%s/%s/%t/%d", token.OwnerID, token.Kind, token.Label, token.Nickname, token.Revoked, token.Approvals))
	}
	for _, d := range s.data.Projects["demo"].Devices {
		devices = append(devices, d.Request.SessionID+"/"+d.Status+"/"+d.ApprovedBy)
	}
	sort.Strings(tokens)
	sort.Strings(devices)
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	raw, _ := json.Marshal(struct {
		Users                     map[string]string
		Tokens, Devices           []string
		HTTP, Scopes, Attachments bool
		Detach, Suspend           int
		Suspended                 bool
	}{users, tokens, devices, s.data.AllowHTTP, bool(s.data.ScopeIssuanceReady), s.attachmentEnabled, f.b.detached, f.b.suspended, f.b.sessions[0].Suspended})
	return string(raw)
}
func TestAPIAdminPermissionParityAndRouteCoverage(t *testing.T) {
	covered := map[string]bool{}
	for _, route := range adminAPIRoutes() {
		for _, actor := range []string{"admin", "owner", "developer", "otherdev", "viewer", "outsider", "disabled"} {
			t.Run(route.Parity+"/"+actor, func(t *testing.T) {
				left, right := newAdminRouteFixture(t), newAdminRouteFixture(t)
				method, path, form, paired := adminParityForm(t, left, route)
				covered[route.Method+route.Path] = true
				if !paired { // No corresponding pre-MR-E panel form.
					if actor == "disabled" {
						if err := right.s.CreateUser("admin", actor, testPassword, "user"); err != nil {
							t.Fatal(err)
						}
					}
					scopes := []string{"read", "write", "approve"}
					if actor == "admin" {
						scopes = append(scopes, "admin")
					}
					raw, _ := issueAdminActor(t, right.s, actor, scopes...)
					_, target := issueAdminActor(t, right.s, "developer", "read")
					if actor == "disabled" {
						if err := right.s.UpdateUser("admin", actor, "user", false, ""); err != nil {
							t.Fatal(err)
						}
					}
					apiPath, body := adminRouteInput(t, right, route)
					apiPath = strings.ReplaceAll(apiPath, "TOKEN-FIXTURE", target.ID)
					w := apiRequest(right.h, route.Method, apiPath, body, raw)
					if (w.Code < 400) != (actor == "admin") {
						t.Fatalf("API-only actor rule status=%d", w.Code)
					}
					return
				}
				for _, f := range []adminRouteFixture{left, right} {
					if actor == "disabled" {
						if err := f.s.CreateUser("admin", actor, testPassword, "user"); err != nil {
							t.Fatal(err)
						}
						if err := f.s.SetProjectMember("owner", "demo", actor, "developer"); err != nil {
							t.Fatal(err)
						}
					}
				}
				client := loginPanelUser(t, left.h, actor)
				scopes := []string{"read", "write", "approve"}
				if actor == "admin" {
					scopes = append(scopes, "admin")
				}
				raw, _ := issueAdminActor(t, right.s, actor, scopes...)
				if actor == "disabled" {
					for _, f := range []adminRouteFixture{left, right} {
						if err := f.s.UpdateUser("admin", actor, "user", false, ""); err != nil {
							t.Fatal(err)
						}
					}
				}
				panelResult := client.call(method, path, form)
				apiPath, body := adminRouteInput(t, right, route)
				apiResult := apiRequest(right.h, route.Method, apiPath, body, raw)
				panelAllowed := panelResult.Code < 400 && !strings.HasPrefix(panelResult.Header().Get("Location"), "/login")
				apiAllowed := apiResult.Code < 400
				if panelAllowed != apiAllowed {
					t.Fatalf("permission drift panel=%d api=%d", panelResult.Code, apiResult.Code)
				}
				if adminParitySnapshot(left) != adminParitySnapshot(right) {
					t.Fatal("panel/API normalized state differs")
				}
			})
		}
	}
	for _, route := range apiRoutes {
		if strings.HasPrefix(route.Parity, "admin-api-") && !covered[route.Method+route.Path] {
			t.Fatal("MR-D route missing parity coverage")
		}
	}
}

func TestAPIAdminApproveOnlyAndReauthBudgets(t *testing.T) {
	f := newAdminRouteFixture(t)
	approve, _ := issueAdminActor(t, f.s, "owner", "approve")
	lookup := marshalAdmin(t, map[string]any{"user_code": f.device.UserCode})
	if w := apiRequest(f.h, "POST", "/api/v1/device-approvals/lookup", lookup, approve); w.Code != 200 {
		t.Fatalf("approve-only lookup=%d", w.Code)
	}
	decision := marshalAdmin(t, map[string]any{"user_code": f.device.UserCode, "action": "approve", "nickname": "approved-only"})
	if w := apiRequest(f.h, "POST", "/api/v1/device-approvals/decision", decision, approve); w.Code != 200 {
		t.Fatal("approve-only decision denied")
	}
	raw, v := issueAdminActor(t, f.s, "admin", "admin")
	f.s.hashSlot <- struct{}{}
	w := apiRequest(f.h, "PATCH", "/api/v1/admin/users/developer", `{"enabled":false}`, raw)
	<-f.s.hashSlot
	if w.Code != 503 || w.Header().Get("Retry-After") == "" || f.s.api.reauth[v.ID].n != 0 {
		t.Fatal("busy charged reauth or lacked retry contract")
	}
	body := `{"role":"admin","current_password":"wrong synthetic password"}`
	for i := range 6 {
		want := 401
		if i == 5 {
			want = 429
		}
		if w := apiRequest(f.h, "PATCH", "/api/v1/admin/users/developer", body, raw); w.Code != want {
			t.Fatalf("reauth budget step%d=%d want%d", i, w.Code, want)
		}
	}
}
func TestAPIAdminAdmittedSessionMayFinishAfterRevoke(t *testing.T) {
	f := newAdminRouteFixture(t)
	f.b.entered = make(chan struct{})
	f.b.release = make(chan struct{})
	raw, v := issueAdminActor(t, f.s, "admin", "write")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- apiRequest(f.h, "POST", "/api/v1/projects/demo/sessions/reg-target/detach", `{}`, raw) }()
	<-f.b.entered
	f.s.mu.Lock()
	err := f.s.revokeUserTokensLocked("admin", v.ID, false)
	f.s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	close(f.b.release)
	w := <-done
	if w.Code != 204 || f.b.detached != 1 {
		t.Fatal("already admitted backend operation was stranded")
	}
}

func TestAPIAdminSessionMembershipCommitBarrier(t *testing.T) {
	f := newAdminRouteFixture(t)
	raw, _ := issueAdminActor(t, f.s, "developer", "write")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	body := &policyReader{Reader: strings.NewReader(`{}`), change: func() { once.Do(func() { close(entered); <-release }) }}
	r := httptest.NewRequest("POST", "https://example.test/api/v1/projects/demo/sessions/reg-target/detach", body)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { f.h.ServeHTTP(w, r); close(done) }()
	<-entered
	if err := f.s.SetProjectMember("owner", "demo", "developer", "viewer"); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if w.Code != 403 || f.b.reads != 0 || f.b.detached != 0 {
		t.Fatal("demoted member reached backend before guarded admission")
	}
}

func TestAPIAdminBackendFailureJSON(t *testing.T) {
	for _, failure := range []string{"sessions", "detach", "suspend"} {
		t.Run(failure, func(t *testing.T) {
			f := newAdminRouteFixture(t)
			f.b.fail = failure
			raw, _ := issueAdminActor(t, f.s, "admin", "write")
			method, path, body := "POST", "/api/v1/projects/demo/sessions/reg-target/detach", `{}`
			want := 409
			if failure == "sessions" {
				want = 503
			}
			if failure == "suspend" {
				method, path, body = "PUT", "/api/v1/projects/demo/sessions/reg-target/suspension", `{"suspended":true}`
			}
			w := apiRequest(f.h, method, path, body, raw)
			if w.Code != want || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("backend refusal=%d want%d", w.Code, want)
			}
		})
	}
}
