package serverruntime

import (
	"encoding/json"
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

type deviceRuntime struct {
	t        *testing.T
	h        http.Handler
	close    func() error
	dir      string
	project  string
	cookie   *http.Cookie
	csrf     string
	password string
}

func newDeviceRuntime(t *testing.T, configure ...func(*controlpanel.Store)) (*deviceRuntime, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := controlpanel.OpenSQLite(filepath.Join(dir, "control"))
	if err != nil {
		t.Fatal(err)
	}
	const password = "test-only-device-password"
	if err := store.Setup(password); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateProject("test"); err != nil {
		t.Fatal(err)
	}
	for _, apply := range configure {
		apply(store)
	}
	projectToken, err := store.IssueProjectTokenFor("admin", "test", "fixture harnesses")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	r := &deviceRuntime{t: t, dir: dir, project: "test", password: password}
	r.start()
	t.Cleanup(func() { _ = r.close() })
	r.login(password)
	return r, projectToken
}

func (r *deviceRuntime) start() {
	r.t.Helper()
	h, closeHandler, err := newManagedHandler(r.dir, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	r.h, r.close = h, closeHandler
}

func (r *deviceRuntime) restart() {
	r.t.Helper()
	if err := r.close(); err != nil {
		r.t.Fatal(err)
	}
	r.start()
	// Browser logins live in the panel's memory; a restarted runtime needs a new one.
	r.login(r.password)
}

func (r *deviceRuntime) bound(token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+token)
		r.h.ServeHTTP(w, req)
	})
}

func (r *deviceRuntime) browser(method, path string, values url.Values) *httptest.ResponseRecorder {
	r.t.Helper()
	if values == nil {
		values = url.Values{}
	}
	if method == http.MethodPost {
		values.Set("csrf", r.csrf)
	}
	req := httptest.NewRequest(method, "http://broker"+path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if r.cookie != nil {
		req.AddCookie(r.cookie)
	}
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	return w
}

func (r *deviceRuntime) login(password string) {
	r.t.Helper()
	csrfRE := regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	page := r.browser(http.MethodGet, "/login", nil)
	r.cookie = page.Result().Cookies()[0]
	r.csrf = csrfRE.FindStringSubmatch(page.Body.String())[1]
	login := r.browser(http.MethodPost, "/login", url.Values{"username": {"admin"}, "password": {password}})
	if login.Code != 303 {
		r.t.Fatal("login", login.Code)
	}
	r.cookie = login.Result().Cookies()[0]
	home := r.browser(http.MethodGet, "/", nil)
	r.csrf = csrfRE.FindStringSubmatch(home.Body.String())[1]
}

func (r *deviceRuntime) rest(path, bearer string, form url.Values) (int, map[string]any) {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "https://broker"+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+bearer)
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

// authorize runs one conversation through code issue, browser approval and
// credential exchange, returning its session credential.
func (r *deviceRuntime) authorize(projectToken string, request broker.RegisterRequest, nickname string) string {
	token, _ := r.authorizeGrant(projectToken, request, nickname)
	return token
}

func (r *deviceRuntime) authorizeGrant(projectToken string, request broker.RegisterRequest, nickname string) (string, string) {
	r.t.Helper()
	status, issued := r.rest("/oauth/device_authorization?project="+r.project, projectToken, url.Values{
		"client_id": {"asb-client-v1"}, "scope": {"asb:session"}, "harness": {request.Harness}, "session_id": {request.SessionID},
		"asb_client_instance_id": {request.ClientID}, "request_id": {request.RequestID},
	})
	if status != 200 {
		r.t.Fatalf("device authorization: %d %v", status, issued)
	}
	approve := r.browser(http.MethodPost, "/auth/code", url.Values{"action": {"approve"}, "code": {issued["user_code"].(string)}, "nickname": {nickname}})
	if approve.Code != 303 {
		r.t.Fatalf("approve %s: %d %s", nickname, approve.Code, approve.Body.String())
	}
	status, granted := r.rest("/oauth/token?project="+r.project, projectToken, url.Values{
		"client_id": {"asb-client-v1"}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {issued["device_code"].(string)},
	})
	token, _ := granted["access_token"].(string)
	if status != 200 || !strings.HasPrefix(token, "asb_s_") || granted["nickname"] != nickname {
		r.t.Fatalf("exchange %s: %d (response omitted)", nickname, status)
	}
	grantID, _ := granted["session_grant_id"].(string)
	return token, grantID
}

func (r *deviceRuntime) mcpStatus(token string) int {
	req := httptest.NewRequest(http.MethodGet, "https://broker/mcp?project="+r.project, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	return w.Code
}

func TestDeviceAuthorizedSessionsRegisterExchangeAndSurviveRestart(t *testing.T) {
	r, projectToken := newDeviceRuntime(t)
	first, second := protectedRequest(1, "ignored"), protectedRequest(2, "ignored")
	firstToken := r.authorize(projectToken, first, "agent-one")
	secondToken := r.authorize(projectToken, second, "agent-two")
	if firstToken == secondToken {
		t.Fatal("two conversations share one session credential")
	}
	if status := r.mcpStatus(projectToken); status != 401 {
		t.Fatalf("project token reached /mcp: %d", status)
	}
	registered := managedTool[broker.RegisterResult](t, r.bound(firstToken), "register_session", first)
	if registered.Code != "" || registered.Session.Name != "agent-one" {
		t.Fatalf("first registration: %+v", registered)
	}
	other := managedTool[broker.RegisterResult](t, r.bound(secondToken), "register_session", second)
	if other.Code != "" || other.Session.Name != "agent-two" {
		t.Fatalf("second registration: %+v", other)
	}
	// A credential only authorizes operations on its own registration.
	swapped := managedTool[broker.RegisterResult](t, r.bound(secondToken), "register_session", first)
	if swapped.Code == "" {
		t.Fatal("second credential registered the first conversation")
	}
	sent := managedTool[broker.SendResult](t, r.bound(firstToken), "send_message", broker.Message{From: "agent-one", To: "agent-two", Body: "hello two"})
	if !sent.Accepted {
		t.Fatalf("send between approved sessions: %+v", sent)
	}
	received := managedTool[broker.Message](t, r.bound(secondToken), "wait_for_message", map[string]string{"recipient": "agent-two", "channelId": second.ChannelID})
	if received.Body != "hello two" || received.From != "agent-one" {
		t.Fatalf("receive: %+v", received)
	}
	r.restart()
	listed := managedTool[[]broker.Session](t, r.bound(firstToken), "list_sessions", map[string]any{})
	if len(listed) != 2 {
		t.Fatalf("registrations after restart: %+v", listed)
	}
	resumed := managedTool[broker.RegisterResult](t, r.bound(secondToken), "resume_session", broker.ResumeRequest{RegistrationID: other.Session.RegistrationID, Harness: second.Harness, SessionID: second.SessionID, PID: second.PID, CWD: second.CWD, ClientID: second.ClientID, ChannelID: "lease-2b", Secret: second.Secret, RequestID: "resume-2", ExpectedRevision: other.Session.Revision})
	if resumed.Code != "" || !resumed.Session.AutomaticDeliveryReady {
		t.Fatalf("resume with the session credential after restart: %+v", resumed)
	}
	// Detaching one approved session from the UI revokes only its grant.
	page := r.browser(http.MethodGet, "/projects/sessions?project=test", nil)
	r.csrf = regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(page.Body.String())[1]
	settings := r.browser(http.MethodGet, "/projects/settings?project=test", nil)
	if !strings.Contains(settings.Body.String(), "fixture harnesses") || !strings.Contains(page.Body.String(), "agent-one") {
		t.Fatal("panel does not list the project token and approved sessions")
	}
	if w := r.browser(http.MethodPost, "/sessions/detach", url.Values{"project": {"test"}, "id": {registered.Session.RegistrationID}}); w.Code != 303 {
		t.Fatalf("detach: %d %s", w.Code, w.Body.String())
	}
	if status := r.mcpStatus(firstToken); status != 401 {
		t.Fatalf("detached grant still authorizes: %d", status)
	}
	if status := r.mcpStatus(secondToken); status == 401 {
		t.Fatal("sibling grant lost with the detached one")
	}
	third := r.authorize(projectToken, protectedRequest(3, "ignored"), "agent-one")
	if status := r.mcpStatus(third); status == 401 {
		t.Fatal("released nickname could not be approved again")
	}
	// Revoking the project token ends every session approved through it.
	var parentID string
	page = r.browser(http.MethodGet, "/projects/settings?project=test", nil)
	for _, match := range regexp.MustCompile(`data-confirm-action="project-token-revoke"[^>]*>.*?name="id" value="([^"]+)"`).FindAllStringSubmatch(page.Body.String(), -1) {
		parentID = match[1]
	}
	if parentID == "" {
		t.Fatal("project token revoke form missing")
	}
	if w := r.browser(http.MethodPost, "/tokens/revoke", url.Values{"project": {"test"}, "id": {parentID}}); w.Code != 303 {
		t.Fatalf("revoke project token: %d", w.Code)
	}
	if r.mcpStatus(secondToken) != 401 || r.mcpStatus(third) != 401 {
		t.Fatal("grants survive their revoked project token")
	}
	if status, body := r.rest("/oauth/device_authorization?project=test", projectToken, url.Values{"client_id": {"asb-client-v1"}, "harness": {"codex"}, "session_id": {"11111111-1111-4111-8111-000000000009"}, "asb_client_instance_id": {"client-0009"}, "request_id": {"request-9"}}); status != 401 || body["error"] != "invalid_client" {
		t.Fatalf("revoked project token still issues codes: %d %v", status, body)
	}
}
