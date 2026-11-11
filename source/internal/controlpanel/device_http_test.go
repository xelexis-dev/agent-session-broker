package controlpanel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

type deviceHTTPFixture struct {
	t     *testing.T
	store *Store
	h     http.Handler
}

func newDeviceHTTPFixture(t *testing.T) *deviceHTTPFixture {
	t.Helper()
	s := userStore(t)
	h := New(s, &panelBackend{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	return &deviceHTTPFixture{t: t, store: s, h: h}
}

// rest performs a client REST call: no browser cookie, bearer only.
func (f *deviceHTTPFixture) rest(method, path, bearer string, form url.Values) (*httptest.ResponseRecorder, map[string]any) {
	f.t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, "https://broker"+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, req)
	var decoded map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &decoded)
	return w, decoded
}

func (f *deviceHTTPFixture) resetPoll() {
	f.store.mu.Lock()
	f.store.devicePoll = nil
	f.store.mu.Unlock()
}

func issuedTokenFrom(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	m := regexp.MustCompile(`<code id="issued-token">([^<]+)</code>`).FindStringSubmatch(w.Body.String())
	if w.Code != 200 || len(m) != 2 {
		t.Fatalf("token not issued: %d %s", w.Code, w.Body.String())
	}
	return m[1]
}

func deviceForm(session, requestID string) url.Values {
	return url.Values{"client_id": {"asb-client-v1"}, "harness": {"claude"}, "session_id": {session}, "asb_client_instance_id": {"pc-example"}, "request_id": {requestID}, "scope": {"asb:session"}}
}

func tokenForm(deviceCode string) url.Values {
	return url.Values{"client_id": {"asb-client-v1"}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {deviceCode}}
}

func TestDeviceRESTIssuesApprovesAndExchangesWithProjectTokenOnly(t *testing.T) {
	f := newDeviceHTTPFixture(t)
	owner := loginPanelUser(t, f.h, "owner")
	projectToken := issuedTokenFrom(t, owner.call("GET", issueProjectResult(t, owner, "shared harnesses"), nil))
	if !strings.HasPrefix(projectToken, "asb_p_") {
		t.Fatalf("project token prefix: %q", projectToken)
	}
	if w, _ := f.rest("POST", "/oauth/device_authorization?project=demo", projectToken, nil); w.Code != 400 {
		t.Fatalf("empty device request = %d", w.Code)
	}
	w, code := f.rest("POST", "/oauth/device_authorization?project=demo", projectToken, deviceForm("11111111-1111-4111-8111-111111111111", "req-1"))
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("device authorization: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	deviceCode, _ := code["device_code"].(string)
	userCode, _ := code["user_code"].(string)
	if deviceCode == "" || userCode == "" || code["verification_uri"] != "/auth/code" || code["expires_in"].(float64) != 600 || code["interval"].(float64) != 5 {
		t.Fatalf("device authorization body: %v", code)
	}
	w, body := f.rest("POST", "/oauth/token?project=demo", projectToken, tokenForm(deviceCode))
	if w.Code != 400 || body["error"] != "authorization_pending" {
		t.Fatalf("pending exchange: %d %v", w.Code, body)
	}
	f.resetPoll()
	w, body = f.rest("POST", "/oauth/token?project=demo", projectToken, tokenForm(deviceCode))
	if w.Code != 400 || body["error"] != "authorization_pending" {
		t.Fatalf("second pending exchange: %d %v", w.Code, body)
	}
	lookup := owner.call("POST", "/auth/code", url.Values{"action": {"lookup"}, "code": {strings.ToLower(userCode)}})
	if lookup.Code != 200 || !strings.Contains(lookup.Body.String(), "claude") || !strings.Contains(lookup.Body.String(), "pc-example") || !strings.Contains(lookup.Body.String(), "11111111-1111-4111-8111-111111111111") {
		t.Fatalf("lookup does not describe the requesting session: %d %s", lookup.Code, lookup.Body.String())
	}
	approve := owner.call("POST", "/auth/code", url.Values{"action": {"approve"}, "code": {userCode}, "nickname": {"agent-a"}})
	if approve.Code != 303 || approve.Header().Get("Location") != "/projects/sessions?project=demo" {
		t.Fatalf("approve: %d %s %s", approve.Code, approve.Header().Get("Location"), approve.Body.String())
	}
	f.resetPoll()
	w, body = f.rest("POST", "/oauth/token?project=demo", projectToken, tokenForm(deviceCode))
	sessionToken, _ := body["access_token"].(string)
	if w.Code != 200 || !strings.HasPrefix(sessionToken, "asb_s_") || body["token_type"] != "Bearer" || body["nickname"] != "agent-a" || body["session_grant_id"] == "" || body["expires_in"].(float64) <= 0 {
		t.Fatalf("exchange: %d %v", w.Code, body)
	}
	if w, _ := f.rest("GET", "/mcp?project=demo", sessionToken, nil); w.Code != 204 {
		t.Fatalf("session credential rejected at /mcp: %d", w.Code)
	}
	if w, _ := f.rest("GET", "/mcp?project=demo", projectToken, nil); w.Code != 401 {
		t.Fatalf("project token accepted at /mcp: %d", w.Code)
	}
	if w, body := f.rest("POST", "/oauth/token?project=demo", sessionToken, tokenForm(deviceCode)); w.Code != 401 || body["error"] != "invalid_client" {
		t.Fatalf("session credential accepted at the token endpoint: %d %v", w.Code, body)
	}
	if w, body := f.rest("POST", "/oauth/device_authorization?project=demo", "asb_p_wrong", deviceForm("22222222-2222-4222-8222-222222222222", "req-2")); w.Code != 401 || body["error"] != "invalid_client" || w.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("unknown project token: %d %v", w.Code, body)
	}
	page := owner.call("GET", "/projects/settings?project=demo", nil)
	html := page.Body.String()
	if !strings.Contains(html, "shared harnesses") || !strings.Contains(html, en("tokens.approval_count", "n", 1)) || strings.Contains(html, sessionToken) || strings.Contains(html, projectToken) {
		t.Fatal("settings page does not describe tokens safely")
	}
	if detail := owner.call("GET", "/projects/sessions?project=demo", nil).Body.String(); strings.Contains(detail, sessionToken) || strings.Contains(detail, projectToken) {
		t.Fatal("sessions page leaks credentials")
	}
}

func TestDeviceRESTRejectsWrongMethodsProjectsAndUnreadyStore(t *testing.T) {
	f := newDeviceHTTPFixture(t)
	if w, _ := f.rest("GET", "/oauth/token?project=demo", "asb_p_x", nil); w.Code != 405 {
		t.Fatalf("GET token endpoint = %d", w.Code)
	}
	if w, body := f.rest("POST", "/oauth/device_authorization", "asb_p_x", deviceForm("11111111-1111-4111-8111-111111111111", "req-1")); w.Code != 400 || body["error"] != "invalid_request" {
		t.Fatalf("missing project = %d %v", w.Code, body)
	}
	if w, body := f.rest("POST", "/oauth/device_authorization?project=demo", "", deviceForm("11111111-1111-4111-8111-111111111111", "req-1")); w.Code != 401 || body["error"] != "invalid_client" {
		t.Fatalf("missing bearer = %d %v", w.Code, body)
	}
	empty, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	unready := &deviceHTTPFixture{t: t, store: empty, h: New(empty, &panelBackend{}, http.NotFoundHandler())}
	if w, body := unready.rest("POST", "/oauth/token?project=demo", "asb_p_x", tokenForm("d")); w.Code != 503 || body["error"] != "server_error" {
		t.Fatalf("unready store = %d %v", w.Code, body)
	}
}

func TestAuthCodeApprovalRequiresLoginCSRFRoleAndUniqueNickname(t *testing.T) {
	f := newDeviceHTTPFixture(t)
	owner := loginPanelUser(t, f.h, "owner")
	projectToken := issuedTokenFrom(t, owner.call("GET", issueProjectResult(t, owner, "shared"), nil))
	_, first := f.rest("POST", "/oauth/device_authorization?project=demo", projectToken, deviceForm("11111111-1111-4111-8111-111111111111", "req-1"))
	_, second := f.rest("POST", "/oauth/device_authorization?project=demo", projectToken, deviceForm("22222222-2222-4222-8222-222222222222", "req-2"))
	firstCode, secondCode := first["user_code"].(string), second["user_code"].(string)

	anonymous := httptest.NewRecorder()
	f.h.ServeHTTP(anonymous, httptest.NewRequest("GET", "http://broker/auth/code", nil))
	if anonymous.Code != 303 || anonymous.Header().Get("Location") != "/login?next=%2Fauth%2Fcode" {
		t.Fatalf("anonymous approval page = %d", anonymous.Code)
	}
	noCSRF := httptest.NewRequest("POST", "http://broker/auth/code", strings.NewReader(url.Values{"action": {"approve"}, "code": {firstCode}, "nickname": {"agent-a"}}.Encode()))
	noCSRF.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	noCSRF.AddCookie(owner.cookie)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, noCSRF)
	if w.Code != 403 {
		t.Fatalf("approval without CSRF = %d", w.Code)
	}
	viewer := loginPanelUser(t, f.h, "viewer")
	if w := viewer.call("POST", "/auth/code", url.Values{"action": {"approve"}, "code": {firstCode}, "nickname": {"agent-a"}}); w.Code != 403 && w.Code != 404 && w.Code != 400 {
		t.Fatalf("viewer approved a session: %d", w.Code)
	}
	developer := loginPanelUser(t, f.h, "developer")
	if w := developer.call("POST", "/auth/code", url.Values{"action": {"lookup"}, "code": {firstCode}}); w.Code == 200 && strings.Contains(w.Body.String(), "11111111-1111-4111-8111-111111111111") {
		t.Fatal("developer can inspect another owner's code")
	}
	if w := owner.call("GET", "/auth/code", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `name="code"`) {
		t.Fatalf("approval page: %d", w.Code)
	}
	if w := owner.call("POST", "/auth/code", url.Values{"action": {"approve"}, "code": {firstCode}, "nickname": {"agent-a"}}); w.Code != 303 {
		t.Fatalf("first approval: %d %s", w.Code, w.Body.String())
	}
	duplicate := owner.call("POST", "/auth/code", url.Values{"action": {"approve"}, "code": {secondCode}, "nickname": {"agent-a"}})
	if duplicate.Code != 409 || !strings.Contains(duplicate.Body.String(), `role="alert"`) || !strings.Contains(duplicate.Body.String(), enHTML("device.error_nickname_taken", "nickname", "agent-a")) || !strings.Contains(duplicate.Body.String(), `name="nickname"`) {
		t.Fatalf("duplicate nickname: %d %s", duplicate.Code, duplicate.Body.String())
	}
	if w := owner.call("POST", "/auth/code", url.Values{"action": {"approve"}, "code": {secondCode}, "nickname": {"agent-b"}}); w.Code != 303 {
		t.Fatalf("retry with a free nickname: %d", w.Code)
	}
	if w := owner.call("POST", "/auth/code", url.Values{"action": {"approve"}, "code": {secondCode}, "nickname": {"agent-c"}}); w.Code == 303 {
		t.Fatal("an approved code was approved again")
	}
	_, third := f.rest("POST", "/oauth/device_authorization?project=demo", projectToken, deviceForm("33333333-3333-4333-8333-333333333333", "req-3"))
	if w := owner.call("POST", "/auth/code", url.Values{"action": {"deny"}, "code": {third["user_code"].(string)}}); w.Code != 303 {
		t.Fatalf("deny: %d", w.Code)
	}
	f.resetPoll()
	if w, body := f.rest("POST", "/oauth/token?project=demo", projectToken, tokenForm(third["device_code"].(string))); w.Code != 400 || body["error"] != "access_denied" {
		t.Fatalf("denied exchange: %d %v", w.Code, body)
	}
	for i := 0; i < 12; i++ {
		w = owner.call("POST", "/auth/code", url.Values{"action": {"lookup"}, "code": {"ZZZZ-ZZZZ"}})
	}
	if w.Code != 429 {
		t.Fatalf("code guessing not limited: %d", w.Code)
	}
}
