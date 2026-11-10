package controlpanel

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func apiFixture(t *testing.T) (*Store, http.Handler) {
	t.Helper()
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.SetAdminAPI("admin", true); err != nil {
		t.Fatal(err)
	}
	return s, New(s, nil, http.NotFoundHandler())
}
func apiRequest(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://example.test"+path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func apiIssue(t *testing.T, s *Store, scopes ...string) (string, UserTokenView) {
	t.Helper()
	raw, view, err := s.issueUserToken("admin", testPassword, "synthetic-label", 0, scopes, "login", "fixture", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	return raw, view
}
func TestAPISwitchCookieAndJSONBoundary(t *testing.T) {
	s, h := apiFixture(t)
	if err := s.SetAdminAPI("admin", false); err != nil {
		t.Fatal(err)
	}
	if w := apiRequest(h, "GET", "/api/v1/openapi.json", "", ""); w.Code != 200 {
		t.Fatalf("spec off=%d", w.Code)
	}
	if w := apiRequest(h, "POST", "/api/v1/login", `{}`, ""); w.Code != 403 {
		t.Fatalf("off=%d", w.Code)
	}
	if err := s.SetAdminAPI("admin", true); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"username":"admin","password":"a","password":"b"}`, `{"unknown":true}`, `{} {}`, `[]`, `null`} {
		if w := apiRequest(h, "POST", "/api/v1/login", body, ""); w.Code != 400 {
			t.Fatalf("invalid JSON status=%d", w.Code)
		}
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"GET", "/api/v1/missing", 404}, {"OPTIONS", "/api/v1/me", 405}, {"PUT", "/api/v1/login", 405}} {
		w := apiRequest(h, tc.method, tc.path, "", "")
		if w.Code != tc.status || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("dispatch=%d", w.Code)
		}
	}
	r := httptest.NewRequest("POST", "https://example.test/api/v1/login", strings.NewReader(`{}`))
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatal("cross origin must be JSON")
	}
	raw, _ := apiIssue(t, s)
	for _, bad := range []string{"asb_" + "p_" + strings.Repeat("X", 52), "asb_" + "s_" + strings.Repeat("X", 52), "invalid"} {
		if w := apiRequest(h, "GET", "/api/v1/me", "", bad); w.Code != 401 {
			t.Fatal("wrong bearer kind")
		}
	}
	if w := apiRequest(h, "GET", "/api/v1/me", "", raw); w.Header().Get("Set-Cookie") != "" {
		t.Fatal("API issued a cookie")
	}
}
func TestAPIScopesDTOAndProfilePassword(t *testing.T) {
	s, h := apiFixture(t)
	read, _ := apiIssue(t, s, "read")
	write, _ := apiIssue(t, s, "write")
	if w := apiRequest(h, "PATCH", "/api/v1/me", `{"display_name":"changed"}`, read); w.Code != 403 {
		t.Fatalf("scope=%d", w.Code)
	}
	if w := apiRequest(h, "GET", "/api/v1/me", "", write); w.Code != 403 {
		t.Fatalf("scope read=%d", w.Code)
	}
	w := apiRequest(h, "PATCH", "/api/v1/me", `{"display_name":"changed","email":"synthetic@example.test"}`, write)
	if w.Code != 200 {
		t.Fatalf("profile=%d", w.Code)
	}
	var me map[string]any
	if json.Unmarshal(w.Body.Bytes(), &me) != nil {
		t.Fatal("JSON")
	}
	if len(me) != 7 || me["display_name"] != "changed" {
		t.Fatal("DTO allowlist")
	}
	if w = apiRequest(h, "PATCH", "/api/v1/me", `{"role":"admin"}`, write); w.Code != 400 {
		t.Fatal("authorization fields accepted")
	}
	if w = apiRequest(h, "POST", "/api/v1/me/password", `{"current_password":"wrong","password":"new synthetic password"}`, write); w.Code != 401 {
		t.Fatalf("reauth=%d", w.Code)
	}
	if w = apiRequest(h, "POST", "/api/v1/me/password", `{"current_password":"`+testPassword+`","password":"new synthetic password"}`, write); w.Code != 204 {
		t.Fatalf("password=%d", w.Code)
	}
	if _, err := s.authenticateAPI(read); !errors.Is(err, errAPIUnauthorized) {
		t.Fatal("revision did not kill sibling token")
	}
	if !s.Authenticate("new synthetic password") {
		t.Fatal("new password")
	}
}
func TestAPITokenPersistenceNoSecretsAndLastUse(t *testing.T) {
	s, _ := apiFixture(t)
	raw, view := apiIssue(t, s)
	records, err := s.listRecords()
	if err != nil {
		t.Fatal(err)
	}
	for key, b := range records {
		if bytes.Contains(b, []byte(raw)) || bytes.Contains(b, []byte(testPassword)) || strings.HasPrefix(key, "api-audit/") && bytes.Contains(b, []byte("synthetic-label")) {
			t.Fatal("secret or label in durable state/audit")
		}
	}
	write := s.writeBatch
	uses := 0
	s.writeBatch = func(puts map[string][]byte, deletes []string) error {
		for key := range puts {
			if strings.HasPrefix(key, "user-tokens/") {
				uses++
			}
		}
		return write(puts, deletes)
	}
	for range 3 {
		if _, err = s.authenticateAPI(raw); err != nil {
			t.Fatal(err)
		}
	}
	if uses != 1 {
		t.Fatalf("last use writes=%d", uses)
	}
	s.writeBatch = write
	// A fresh index loaded from the exact durable rows authenticates the same token.
	records, _ = s.listRecords()
	s.mu.Lock()
	s.loadAPI(records)
	s.mu.Unlock()
	if a, err := s.authenticateAPI(raw); err != nil || a.tokenID != view.ID {
		t.Fatal("durable token")
	}
	if _, err = s.ListOwnUserTokens("admin"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	if err = s.revokeUserTokensLocked("admin", view.ID, false); err != nil {
		t.Fatal(err)
	}
	s.mu.Unlock()
	if _, err = s.authenticateAPI(raw); err == nil {
		t.Fatal("revoked token accepted")
	}
	records, _ = s.listRecords()
	for key := range records {
		if strings.Contains(key, view.ID) {
			t.Fatal("late last_used resurrected row")
		}
	}
}
func TestAPIBusyLimitersAndReauthBudget(t *testing.T) {
	s, h := apiFixture(t)
	s.hashSlot <- struct{}{}
	w := apiRequest(h, "POST", "/api/v1/login", `{"username":"missing","password":"wrong"}`, "")
	if w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("busy=%d", w.Code)
	}
	if s.api.global.n != 0 {
		t.Fatal("busy charged login")
	}
	<-s.hashSlot
	for i := 0; i < 6; i++ {
		w = apiRequest(h, "POST", "/api/v1/login", `{"username":"missing","password":"wrong"}`, "")
		want := 401
		if i == 5 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("attempt=%d status=%d", i, w.Code)
		}
	}
	s.api.loginPeers = map[string]apiRate{}
	s.api.global = apiRate{}
	for i := 0; i < 11; i++ {
		s.mu.Lock()
		allowed := s.apiLoginAllowedLocked(rand.Text())
		s.mu.Unlock()
		if allowed != (i < 10) {
			t.Fatal("global login limit")
		}
	}
	// Limiter expiry/map cap and no arbitrary eviction of a current budget.
	m := map[string]apiRate{}
	now := s.now()
	for range 1100 {
		rateAllowed(m, rand.Text(), now, 5, time.Minute)
	}
	if len(m) != 1024 {
		t.Fatal("limiter map cap")
	}
	rateAllowed(m, "new", now.Add(2*time.Minute), 5, time.Minute)
	if len(m) != 1 {
		t.Fatal("limiter expiry")
	}
	s.api.loginPeers = map[string]apiRate{}
	s.api.global = apiRate{}
	raw, _ := apiIssue(t, s, "write")
	for i := 0; i < 6; i++ {
		w = apiRequest(h, "POST", "/api/v1/me/password", `{"current_password":"wrong","password":"another synthetic password"}`, raw)
		want := 401
		if i == 5 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("reauth attempt=%d status=%d", i, w.Code)
		}
	}
}
func TestAPIWriteLimitIsPerUser(t *testing.T) {
	s, h := apiFixture(t)
	a, _ := apiIssue(t, s, "write")
	b, _ := apiIssue(t, s, "write")
	for i := 0; i < 61; i++ {
		raw := a
		if i%2 == 0 {
			raw = b
		}
		w := apiRequest(h, "PATCH", "/api/v1/me", `{}`, raw)
		want := 200
		if i == 60 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("write=%d status=%d", i, w.Code)
		}
	}
}
func TestAPITransportAtAllIssuanceBoundaries(t *testing.T) {
	s, h := apiFixture(t)
	r := httptest.NewRequest("POST", "http://example.test/api/v1/login", panicReader{})
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("transport before body")
	}
	if err := s.SetInstanceHTTPPolicy("admin", true); err != nil {
		t.Fatal(err)
	}
	s.beforeTokenExchange = func() {
		if err := s.SetInstanceHTTPPolicy("admin", false); err != nil {
			t.Error(err)
		}
	}
	_, _, err := s.issueUserToken("admin", testPassword, "", 0, nil, "login", "fixture", true, 0)
	if !errors.Is(err, errHTTPNotAllowed) {
		t.Fatal("policy changed during KDF")
	}
	s.beforeTokenExchange = nil
	admin, _ := s.User("admin")
	if _, _, err = s.IssueOwnUserToken("admin", admin.Revision, "", 0, nil, "panel-digest", true); !errors.Is(err, errHTTPNotAllowed) {
		t.Fatal("panel plaintext")
	}
	if _, _, err = s.IssueOwnUserToken("admin", admin.Revision, "", 0, nil, "panel-digest", false); err != nil {
		t.Fatal("panel HTTPS")
	}
	// Policy can also change while the request body is being read.
	if err = s.SetInstanceHTTPPolicy("admin", true); err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRequest("POST", "http://example.test/api/v1/login", &policyReader{Reader: strings.NewReader(`{"username":"admin","password":"` + testPassword + `"}`), change: func() { _ = s.SetInstanceHTTPPolicy("admin", false) }})
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("post body policy=%d", w.Code)
	}
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) {
	panic("credential body read before transport admission")
}

type policyReader struct {
	io.Reader
	change func()
	once   sync.Once
}

func (r *policyReader) Read(b []byte) (int, error) { r.once.Do(r.change); return r.Reader.Read(b) }

func TestAPICommitGuardInterleavings(t *testing.T) {
	for _, change := range []string{"revoke", "password", "switch", "expiry", "disabled", "scope"} {
		t.Run(change, func(t *testing.T) {
			s, _ := apiFixture(t)
			raw, v := apiIssue(t, s)
			a, err := s.authenticateAPI(raw)
			if err != nil {
				t.Fatal(err)
			}
			admitted := make(chan struct{})
			release := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				close(admitted)
				<-release
				s.mu.Lock()
				defer s.mu.Unlock()
				done <- a.guardLocked(s, "write")()
			}()
			<-admitted
			s.mu.Lock()
			switch change {
			case "revoke":
				_ = s.revokeUserTokensLocked("admin", v.ID, false)
			case "password":
				u := s.data.Users["admin"]
				u.Revision++
				s.data.Users["admin"] = u
			case "switch":
				s.api.enabled = false
			case "expiry":
				token := s.api.tokens[v.ID]
				token.ExpiresAt = s.now().Add(-time.Second)
				s.api.tokens[v.ID] = token
			case "disabled":
				u := s.data.Users["admin"]
				u.Enabled = false
				s.data.Users["admin"] = u
			case "scope":
				token := s.api.tokens[v.ID]
				token.Scopes = []string{"read"}
				s.api.tokens[v.ID] = token
			}
			s.mu.Unlock()
			close(release)
			if err := <-done; err == nil {
				t.Fatal("stale principal passed commit guard")
			}
		})
	}
}
func TestAPIReductionsMemoryFirstAndCurrentSwitchRetry(t *testing.T) {
	s, _ := apiFixture(t)
	raw, v := apiIssue(t, s)
	write := s.writeBatch
	s.writeBatch = func(map[string][]byte, []string) error { return ErrStorage }
	s.mu.Lock()
	err := s.revokeUserTokensLocked("admin", v.ID, false)
	s.mu.Unlock()
	if !errors.Is(err, ErrStorage) {
		t.Fatal("revoke storage")
	}
	if _, err = s.authenticateAPI(raw); err == nil {
		t.Fatal("failed persistence reenabled token")
	}
	if err = s.SetAdminAPI("admin", false); !errors.Is(err, ErrStorage) || s.AdminAPIEnabled() {
		t.Fatal("OFF failed open")
	}
	s.writeBatch = write
	if err = s.SetAdminAPI("admin", true); err != nil {
		t.Fatal(err)
	}
	if err = s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	records, _ := s.listRecords()
	var setting struct {
		Enabled bool `json:"enabled"`
	}
	_ = json.Unmarshal(records["settings/admin-api"], &setting)
	if !setting.Enabled {
		t.Fatal("stale OFF retry overwrote ON")
	}
	if _, ok := records["user-tokens/admin/"+v.ID]; ok {
		t.Fatal("revoke not retried")
	}
}
func TestAPIRevokeAllExactTargetsIncludingUnknownVersions(t *testing.T) {
	s, _ := apiFixture(t)
	raw, _ := apiIssue(t, s)
	rows := map[string][]byte{"user-tokens/admin/unknown": []byte(`{"v":99}`), "user-tokens/Admin/other": []byte(`{"v":99}`), "user-tokens/admin_/other": []byte(`{"v":99}`)}
	if s.writeBatch(rows, nil) != nil {
		t.Fatal("fixture")
	}
	write := s.writeBatch
	s.writeBatch = func(map[string][]byte, []string) error { return ErrStorage }
	if err := s.RevokeOwnUserTokens("admin"); !errors.Is(err, ErrStorage) {
		t.Fatal("failed revoke all")
	}
	if _, err := s.authenticateAPI(raw); err == nil {
		t.Fatal("memory revoke all")
	}
	s.writeBatch = write
	// Issuance first retries the exact old delete set; it must never target the new token.
	fresh, v := apiIssue(t, s)
	if err := s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.authenticateAPI(fresh); err != nil {
		t.Fatal("retry killed new issuance")
	}
	records, _ := s.listRecords()
	if _, ok := records["user-tokens/admin/unknown"]; ok {
		t.Fatal("unknown row retained")
	}
	for _, key := range []string{"user-tokens/Admin/other", "user-tokens/admin_/other", "user-tokens/admin/" + v.ID} {
		if _, ok := records[key]; !ok {
			t.Fatal("wrong exact delete target")
		}
	}
}
func TestAPILoadValidationCapsAndRevisionSweep(t *testing.T) {
	s, _ := apiFixture(t)
	raw, v := apiIssue(t, s)
	records, _ := s.listRecords()
	records["user-tokens/admin/unknown"] = []byte(`{"v":99}`)
	token := s.api.tokens[v.ID]
	token.Hash = "bad"
	bad, _ := json.Marshal(token)
	records["user-tokens/admin/malformed"] = bad
	s.mu.Lock()
	s.loadAPI(records)
	s.mu.Unlock()
	if s.api.malformed != 2 {
		t.Fatal("malformed count")
	}
	if _, err := s.authenticateAPI(raw); err != nil {
		t.Fatal("valid row lost")
	}
	s.mu.Lock()
	u := s.data.Users["admin"]
	u.Revision++
	s.data.Users["admin"] = u
	s.mu.Unlock()
	if err := s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	records, _ = s.listRecords()
	if _, ok := records["user-tokens/admin/"+v.ID]; ok {
		t.Fatal("revision row retained")
	}
	// Durable unknown rows consume the durable cap and are removable by exact revoke-all.
	s.mu.Lock()
	for i := 0; i < 2048; i++ {
		s.api.rows["user-tokens/admin/"+rand.Text()] = true
	}
	s.mu.Unlock()
	if _, _, err := s.issueUserToken("admin", testPassword, "", 0, nil, "login", "other", false, 0); !errors.Is(err, errAPITokenCap) {
		t.Fatal("durable cap")
	}
}
func TestAPIAuditBatchCapsAggregatesAndDTO(t *testing.T) {
	s, h := apiFixture(t)
	raw, _ := apiIssue(t, s)
	write := s.writeBatch
	paired := false
	s.writeBatch = func(puts map[string][]byte, deletes []string) error {
		_, state := puts["state"]
		audit := false
		for key := range puts {
			audit = audit || strings.HasPrefix(key, "api-audit/")
		}
		if state {
			paired = audit
		}
		return write(puts, deletes)
	}
	if w := apiRequest(h, "PATCH", "/api/v1/me", `{"display_name":"audit test"}`, raw); w.Code != 200 {
		t.Fatalf("write=%d", w.Code)
	}
	if !paired {
		t.Fatal("state/audit split")
	}
	s.writeBatch = write
	s.mu.Lock()
	s.api.refusals["login_failed"] = 7
	s.api.lastFlush = time.Time{}
	if err := s.sweepAPILocked(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4097; i++ {
		key := "api-audit/success-" + rand.Text()
		s.api.audits[key] = apiAuditEvent{Action: "profile_update"}
	}
	for i := 0; i < 1025; i++ {
		key := "api-audit/refusal-" + rand.Text()
		s.api.audits[key] = apiAuditEvent{Action: "write_refused", Count: 1}
	}
	if err := s.apiBatchLocked(map[string][]byte{}, nil, apiAuditEvent{}); err != nil {
		t.Fatal(err)
	}
	success, refused := 0, 0
	for _, e := range s.api.audits {
		if e.Count > 0 {
			refused++
		} else {
			success++
		}
	}
	s.mu.Unlock()
	if success != 4096 || refused != 1024 {
		t.Fatalf("caps %d/%d", success, refused)
	}
	for _, path := range []string{"/api/v1/me", "/api/v1/me/tokens"} {
		w := apiRequest(h, "GET", path, "", raw)
		for _, field := range []string{"hash", "rev", "password", "autoBindings", "autoExchanges", "boundIdentity"} {
			var v map[string]json.RawMessage
			_ = json.Unmarshal(w.Body.Bytes(), &v)
			if _, ok := v[field]; ok {
				t.Fatal("internal DTO field")
			}
		}
	}
}
func TestAPIOpenAPIRouteAndDTOParity(t *testing.T) {
	var spec struct {
		Paths map[string]map[string]struct {
			Scope       string `json:"x-asb-scope"`
			RequestBody struct {
				Content map[string]struct {
					Schema map[string]string `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
			Responses map[string]struct {
				Content map[string]struct {
					Schema map[string]string `json:"schema"`
				} `json:"content"`
			} `json:"responses"`
		}
		Components struct {
			Schemas map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if json.Unmarshal(adminOpenAPI, &spec) != nil {
		t.Fatal("invalid spec")
	}
	count := 0
	for _, methods := range spec.Paths {
		count += len(methods)
	}
	if count != len(apiRoutes) {
		t.Fatal("route sets differ")
	}
	tags := func(typ reflect.Type) []string {
		out := []string{}
		var walk func(reflect.Type)
		walk = func(v reflect.Type) {
			for i := 0; i < v.NumField(); i++ {
				f := v.Field(i)
				if f.Anonymous {
					walk(f.Type)
					continue
				}
				out = append(out, strings.Split(f.Tag.Get("json"), ",")[0])
			}
		}
		walk(typ)
		sort.Strings(out)
		return out
	}
	for _, route := range apiRoutes {
		op, ok := spec.Paths[route.Path][strings.ToLower(route.Method)]
		if !ok || op.Scope != route.Scope {
			t.Fatal("route/scope mismatch")
		}
		for _, code := range []string{"400", "401", "403", "404", "405", "409", "429", "503"} {
			if _, ok := op.Responses[code]; !ok {
				t.Fatal("missing error contract")
			}
		}
		responseCode := "200"
		if route.Status == 201 {
			responseCode = "201"
		}
		if route.Status == 204 {
			responseCode = "204"
		}
		for _, pair := range []struct {
			typ reflect.Type
			ref string
		}{{route.Request, op.RequestBody.Content["application/json"].Schema["$ref"]}, {route.Response, op.Responses[responseCode].Content["application/json"].Schema["$ref"]}} {
			if pair.typ == nil {
				continue
			}
			properties := spec.Components.Schemas[strings.TrimPrefix(pair.ref, "#/components/schemas/")].Properties
			keys := []string{}
			for k := range properties {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if !reflect.DeepEqual(tags(pair.typ), keys) {
				t.Fatalf("DTO mismatch %v", pair.typ)
			}
		}
	}
}

// Ensure copy-on-write updates do not mutate a snapshot held by a reader.
func TestAPIProfileCopyOnWrite(t *testing.T) {
	s, h := apiFixture(t)
	old := maps.Clone(s.data.Users)
	raw, _ := apiIssue(t, s, "write")
	w := apiRequest(h, "PATCH", "/api/v1/me", `{"display_name":"new"}`, raw)
	if w.Code != 200 || old["admin"].DisplayName != "" {
		t.Fatal("profile snapshot mutated")
	}
}

func TestAPIActiveAndServerCaps(t *testing.T) {
	for _, server := range []bool{false, true} {
		t.Run(map[bool]string{false: "user", true: "server"}[server], func(t *testing.T) {
			s, _ := apiFixture(t)
			now := s.now().UTC()
			s.mu.Lock()
			n := 32
			if server {
				n = 1024
			}
			for i := 0; i < n; i++ {
				user := "admin"
				if server {
					user = "u" + string(rune('A'+i/16))
					u := s.data.Users["admin"]
					u.ID = user
					u.Username = user
					s.data.Users[user] = u
				}
				id := rand.Text() + rand.Text()
				token := userAPIToken{V: 1, ID: id, Hash: strings.Repeat("0", 64), User: user, Rev: 1, Scopes: []string{"read"}, Source: "panel", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
				s.api.tokens[id] = token
				s.api.rows[token.key()] = true
			}
			s.mu.Unlock()
			if _, _, err := s.issueUserToken("admin", testPassword, "", 0, nil, "login", "cap", false, 0); !errors.Is(err, errAPITokenCap) {
				t.Fatal("active cap not enforced")
			}
		})
	}
}
func TestAPIGrantDoesNotPublishOnFailedPersistence(t *testing.T) {
	s, _ := apiFixture(t)
	s.writeBatch = func(map[string][]byte, []string) error { return ErrStorage }
	if _, _, err := s.issueUserToken("admin", testPassword, "", 0, nil, "login", "fixture", false, 0); !errors.Is(err, ErrStorage) {
		t.Fatal("issuance storage")
	}
	if len(s.api.tokens) != 0 {
		t.Fatal("grant published before commit")
	}
}
func TestAPIPasswordFailureKeepsPasswordAndRevokesTokens(t *testing.T) {
	s, h := apiFixture(t)
	raw, _ := apiIssue(t, s, "write")
	write := s.writeBatch
	before := s.data.Users["admin"]
	s.writeBatch = func(map[string][]byte, []string) error { return ErrStorage }
	w := apiRequest(h, "POST", "/api/v1/me/password", `{"current_password":"`+testPassword+`","password":"replacement synthetic password"}`, raw)
	if w.Code != 503 {
		t.Fatalf("password persistence=%d", w.Code)
	}
	if _, err := s.authenticateAPI(raw); err == nil {
		t.Fatal("failed reduction unblocked bearer")
	}
	if s.data.Users["admin"] != before {
		t.Fatal("failed password commit published candidate")
	}
	s.writeBatch = write
	if err := s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	if !s.Authenticate(testPassword) || s.Authenticate("replacement synthetic password") {
		t.Fatal("failed password change altered credential")
	}
	records, err := s.listRecords()
	if err != nil {
		t.Fatal(err)
	}
	var durable state
	if decodeSQLiteControlState(records["state"], &durable) != nil || durable.Users["admin"] != before {
		t.Fatal("sweep persisted an uncommitted password")
	}
	s.mu.Lock()
	s.loadAPI(records)
	s.mu.Unlock()
	if _, err = s.authenticateAPI(raw); err == nil {
		t.Fatal("token reduction was not retried durably")
	}

}
func TestAPIRouteSmoke(t *testing.T) {
	for _, route := range apiRoutes {
		if strings.HasPrefix(route.Parity, "project-") || strings.HasPrefix(route.Parity, "member-") || route.Parity == "token-catalog" {
			continue
		} // Covered by TestAPIProjectRouteSmoke.
		t.Run(route.Method+route.Path, func(t *testing.T) {
			if strings.HasPrefix(route.Parity, "admin-api-") {
				apiAdminRouteSmoke(t, route)
				return
			}
			s, h := apiFixture(t)
			raw, v := apiIssue(t, s)
			path := strings.ReplaceAll(route.Path, "{id}", v.ID)
			body := ""
			switch route.Parity {
			case "login":
				body = `{"username":"admin","password":"` + testPassword + `"}`
			case "profile":
				if route.Method == "PATCH" {
					body = `{}`
				}
			case "profile-password":
				body = `{"current_password":"` + testPassword + `","password":"replacement synthetic password"}`
			}
			w := apiRequest(h, route.Method, path, body, raw)
			if w.Code != route.Status {
				t.Fatalf("route status=%d want=%d", w.Code, route.Status)
			}
			if route.Response != nil {
				var object map[string]json.RawMessage
				if json.Unmarshal(w.Body.Bytes(), &object) != nil {
					t.Fatal("response JSON")
				}
				var properties func(reflect.Type) int
				properties = func(typ reflect.Type) int {
					n := 0
					for i := 0; i < typ.NumField(); i++ {
						field := typ.Field(i)
						if field.Anonymous {
							n += properties(field.Type)
						} else {
							n++
						}
					}
					return n
				}
				if len(object) != properties(route.Response) {
					t.Fatal("response key allowlist")
				}
			}
		})
	}
}

func TestAPIHandlerCommitBarrier(t *testing.T) {
	for _, kind := range []string{"revoke", "password", "switch", "transport"} {
		t.Run(kind, func(t *testing.T) {
			s, h := apiFixture(t)
			raw, view := apiIssue(t, s, "write")
			scheme := "https"
			if kind == "transport" {
				scheme = "http"
				if err := s.SetInstanceHTTPPolicy("admin", true); err != nil {
					t.Fatal(err)
				}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			body := &policyReader{Reader: strings.NewReader(`{"display_name":"must not commit"}`), change: func() { once.Do(func() { close(entered); <-release }) }}
			r := httptest.NewRequest("PATCH", scheme+"://example.test/api/v1/me", body)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+raw)
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { h.ServeHTTP(w, r); close(done) }()
			<-entered
			switch kind {
			case "revoke":
				s.mu.Lock()
				err := s.revokeUserTokensLocked("admin", view.ID, false)
				s.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			case "password":
				if err := s.ChangeOwnPassword("admin", testPassword, "changed synthetic password", "changed synthetic password"); err != nil {
					t.Fatal(err)
				}
			case "switch":
				if err := s.SetAdminAPI("admin", false); err != nil {
					t.Fatal(err)
				}
			case "transport":
				if err := s.SetInstanceHTTPPolicy("admin", false); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			<-done
			if w.Code != 401 && w.Code != 403 {
				t.Fatalf("stale handler committed=%d", w.Code)
			}
			u, _ := s.User("admin")
			if u.DisplayName != "" {
				t.Fatal("stale mutation reached state")
			}
		})
	}
}
func TestAPIConcurrentLastUseCannotResurrectRevoke(t *testing.T) {
	s, _ := apiFixture(t)
	raw, v := apiIssue(t, s)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _, _ = s.authenticateAPI(raw) }()
	}
	close(start)
	s.mu.Lock()
	err := s.revokeUserTokensLocked("admin", v.ID, false)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	records, _ := s.listRecords()
	if _, ok := records["user-tokens/admin/"+v.ID]; ok {
		t.Fatal("last use resurrected revoked credential")
	}
}
func TestAPICrossOriginLeavesPanelErrorUnchanged(t *testing.T) {
	_, h := apiFixture(t)
	r := httptest.NewRequest("POST", "https://example.test/profile/save", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	expected := httptest.NewRecorder()
	http.NewCrossOriginProtection().Handler(http.NotFoundHandler()).ServeHTTP(expected, r)
	actual := httptest.NewRecorder()
	h.ServeHTTP(actual, r)
	if actual.Code != expected.Code || actual.Body.String() != expected.Body.String() {
		t.Fatal("panel denial contract changed")
	}
}

func TestAPIValidPanelCookieIsIgnored(t *testing.T) {
	_, h := apiFixture(t)
	client := loginPanelUser(t, h, "admin")
	r := httptest.NewRequest("GET", "https://example.test/api/v1/me", nil)
	r.AddCookie(client.cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("panel cookie authorized API")
	}
}
func TestAPISecretsNeverReachLogs(t *testing.T) {
	s, h := apiFixture(t)
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	raw, _ := apiIssue(t, s)
	_ = apiRequest(h, "GET", "/api/v1/me", "", raw)
	if strings.Contains(logs.String(), raw) || strings.Contains(logs.String(), testPassword) {
		t.Fatal("API secret reached log")
	}
}

func TestAPIStagedProductionCloneRejectsSourceBearer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-server refresh is a POSIX developer tool")
	}
	source := t.TempDir()
	s, err := OpenSQLite(source)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.SetAdminAPI("admin", true); err != nil {
		t.Fatal(err)
	}
	raw, _ := apiIssue(t, s)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	clone := t.TempDir()
	data, err := os.ReadFile(filepath.Join(source, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(clone, "control.db"), data, 0600); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../../scripts/dev/refresh-test-server.sh")
	if err != nil {
		t.Fatal(err)
	}
	requirePrivateScript(t, script)
	command := exec.Command("bash", "-c", `source "$1"; strip_management_credentials "$2"`, "fixture", script, clone)
	if command.Run() != nil {
		t.Fatal("synthetic clone scrub failed")
	}
	copied, err := OpenSQLite(clone)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	if w := apiRequest(New(copied, nil, http.NotFoundHandler()), "GET", "/api/v1/me", "", raw); w.Code != 401 {
		t.Fatalf("source token on staged clone=%d", w.Code)
	}
	s, err = OpenSQLite(source)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.authenticateAPI(raw); err != nil {
		t.Fatal("source credential did not survive restart/scrub")
	}
}

func TestAPIErrorContract(t *testing.T) {
	_, h := apiFixture(t)
	for _, tc := range []struct {
		body, media, code string
		status            int
	}{{`{`, "application/json", "invalid_json", 400}, {`{"unknown":1}`, "application/json", "unknown_field", 400}, {`{}`, "text/plain", "unsupported_media_type", 415}, {strings.Repeat(" ", 16385), "application/json", "payload_too_large", 413}, {`{"username":"missing","password":"wrong"}`, "application/json", "invalid_credentials", 401}} {
		r := httptest.NewRequest("POST", "https://example.test/api/v1/login", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", tc.media)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var e apiErrorView
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != tc.status || e.Error != tc.code {
			t.Fatalf("contract status=%d code=%s", w.Code, e.Error)
		}
	}
	w := apiRequest(h, "GET", "/api/v1/me", "", "")
	if w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatal("bearer challenge")
	}
}

func TestAPIWriteRefusalCountedOnce(t *testing.T) {
	s, h := apiFixture(t)
	raw, _ := apiIssue(t, s, "read")
	w := apiRequest(h, "PATCH", "/api/v1/me", `{}`, raw)
	if w.Code != 403 {
		t.Fatalf("scope refusal=%d", w.Code)
	}
	s.mu.RLock()
	count := s.api.refusals["write_refused"]
	s.mu.RUnlock()
	if count != 1 {
		t.Fatalf("one refusal counted %d times", count)
	}
}

// requirePrivateScript skips only when a private repository script is confirmed
// absent, as in the public source export. Private CI must always run the test.
func requirePrivateScript(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if os.Getenv("GITLAB_CI") != "" {
			t.Fatalf("private CI must provide %s", filepath.Base(path))
		}
		t.Skip("private repository script is not part of the public export")
	}
	if err != nil {
		t.Fatal(err)
	}
}
