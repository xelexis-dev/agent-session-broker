package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverruntime"
)

type countingTransport struct {
	next  http.RoundTripper
	codes atomic.Int32
}

type policyCountingTransport struct {
	next     http.RoundTripper
	policies atomic.Int32
	bearers  atomic.Int32
}

func (t *policyCountingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/oauth/transport-policy" {
		t.policies.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			return nil, fmt.Errorf("policy request carried credentials")
		}
	}
	if r.URL.Path == "/oauth/device_authorization" {
		t.bearers.Add(1)
	}
	return t.next.RoundTrip(r)
}

func TestProjectTokenClientAuthorizesAgainstManagedRuntimeOverPolicyGuardedHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	// Register cancellation first so the server's shutdown and owned HTTP
	// transport cleanup run while requests can still finish normally.
	t.Cleanup(cancel)
	root := t.TempDir()
	store, err := controlpanel.OpenSQLite(filepath.Join(root, "control"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Setup("integration-fixture-password"); err != nil {
		t.Fatal(err)
	}
	if err = store.SaveProject("devices", "token", true, ""); err != nil {
		t.Fatal(err)
	}
	if err = store.SetInstanceHTTPPolicy("admin", true); err != nil {
		t.Fatal(err)
	}
	projectToken, err := store.IssueProjectTokenFor("admin", "devices", "integration")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	_ = reservation.Close()
	serverCtx, stopServer := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serverruntime.Run(serverCtx, []string{"--listen", address, "--memory-dir", root}, "test", io.Discard)
	}()
	t.Cleanup(func() {
		stopServer()
		select {
		case err := <-serverDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(7 * time.Second):
			t.Error("server did not stop")
		}
	})
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	backend := "http://" + address
	health := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	waitFor(t, "runtime health", func() bool {
		res, err := health.Get(backend + "/healthz")
		if err != nil {
			return false
		}
		defer res.Body.Close()
		return res.StatusCode == http.StatusOK
	})
	counting := &policyCountingTransport{next: transport}
	c := newDeviceAuthClient(backend+"/mcp?project=devices", projectToken, &http.Client{Transport: counting, Timeout: 5 * time.Second})
	issued, err := c.requestCode(ctx, deviceCodeRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111112304", ClientID: "pc-one", RequestID: "request-304"})
	if err != nil || issued.DeviceCode == "" {
		t.Fatalf("policy-authorized HTTP code request: %+v %v", issued, err)
	}
	if counting.policies.Load() != 1 || counting.bearers.Load() != 1 {
		t.Fatalf("policy=%d bearer=%d", counting.policies.Load(), counting.bearers.Load())
	}
}

func (t *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/oauth/device_authorization" {
		t.codes.Add(1)
	}
	return t.next.RoundTrip(r)
}

// This test runs the production managed runtime behind a TLS reverse proxy
// and drives the real project-token client against it: code request, browser
// approval, credential exchange, registration, delivery, UI detach (401) and
// an explicit re-authorization. No operational broker or token is involved.
func TestProjectTokenClientAuthorizesAgainstManagedRuntimeOverTLS(t *testing.T) {
	testManagedRuntimeTLS(t, false)
}

func TestProjectTokenClientAuthorizesAfterTransientPollFailures(t *testing.T) {
	testManagedRuntimeTLS(t, true)
}

func testManagedRuntimeTLS(t *testing.T, inject bool) {
	polls := newDevicePollTrace()
	defer polls.report(t)
	diagnostics := newTLSDiagnostics()
	diagnostics.step("test", "setup")
	t.Cleanup(func() { diagnostics.report(t.Failed(), t.Logf) })
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	defer cancel()
	root := t.TempDir()
	store, err := controlpanel.OpenSQLite(filepath.Join(root, "control"))
	if err != nil {
		t.Fatal(err)
	}
	const password = "integration-fixture-password"
	if err = store.Setup(password); err != nil {
		t.Fatal(err)
	}
	if err = store.SaveProject("devices", "token", true, ""); err != nil {
		t.Fatal(err)
	}
	projectToken, err := store.IssueProjectTokenFor("admin", "devices", "integration")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	reservation.Close()
	serverCtx, stopServer := context.WithCancel(ctx)
	serverDone := make(chan error, 1)
	diagnostics.step("runtime", "serve")
	go func() {
		serverDone <- serverruntime.Run(serverCtx, []string{"--listen", address, "--memory-dir", root, "--trusted-proxy", "127.0.0.1/32"}, "test", io.Discard)
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			diagnostics.step("runtime", "shutdown")
			stopServer()
			select {
			case err := <-serverDone:
				if err != nil {
					diagnostics.report(true, t.Logf)
					t.Errorf("server shutdown: %v", err)
				}
				diagnostics.step("runtime", "")
			case <-time.After(7 * time.Second):
				diagnostics.report(true, t.Logf)
				t.Error("server did not stop")
			}
		})
	}
	t.Cleanup(stop)
	backend := "http://" + address
	plain := &http.Client{Timeout: 2 * time.Second}
	diagnostics.step("test", "runtime-health")
	waitFor(t, "runtime health", func() bool {
		res, err := plain.Get(backend + "/healthz")
		if err != nil {
			return false
		}
		defer res.Body.Close()
		return res.StatusCode == http.StatusOK
	})
	target, _ := url.Parse(backend)
	reverse := httputil.NewSingleHostReverseProxy(target)
	tls := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Forwarded-Proto", "https")
		polls.serve(w, r, reverse)
	}))
	tls.Config.ConnState = diagnostics.connState
	tls.StartTLS()
	defer tls.Close()
	defer func() { diagnostics.report(t.Failed(), t.Logf) }()
	counting := &countingTransport{next: tls.Client().Transport}
	restClient := &http.Client{Transport: counting, Timeout: 5 * time.Second}
	endpoint := tls.URL + "/mcp?project=devices"
	if _, err := parseClientOptions([]string{"--broker-url", endpoint, "--token", projectToken}); err != nil {
		t.Fatalf("real project token rejected by option parsing: %v", err)
	}

	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	identity := broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111112286", PID: 2286, CWD: t.TempDir()}
	sink := make(chan broker.Message, 1)
	localCtx, stopLocal := context.WithCancel(ctx)
	options := durableOptions{enabled: true, tokenMode: true, state: state, scope: brokerScope(endpoint, "devices"), project: "devices", heartbeat: 50 * time.Millisecond, timeout: 2 * time.Second, backoff: func(int) time.Duration { return 20 * time.Millisecond }}
	reapproval := &reapprovalDiagnostics{}
	options.bindingCreated = reapproval.bindingCreated
	defer reapproval.report(t, state)
	options.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return identity, nil }
	options.device = &deviceAuthOptions{
		authorizer: newDeviceAuthClient(endpoint, projectToken, restClient),
		// The runtime enforces its 5 second polling interval with slow_down, so
		// the client's real pacing is used here.
		clock: realPollClock(),
		newUpstream: func(credential string) Upstream {
			u := newTestBrokerUpstream(localCtx, endpoint)
			u.transport = tls.Client().Transport
			u.setBearerToken(credential)
			return u
		},
	}
	local, shutdown := newIngressServerWithOptions(localCtx, unavailableUpstream{}, snapshotTools(),
		func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) { return identity, nil },
		func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
			return func(ctx context.Context, message broker.Message) error {
				select {
				case sink <- message:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}, nil, nil
		}, options)
	t.Cleanup(shutdown)
	t.Cleanup(stopLocal)
	st, ct := mcp.NewInMemoryTransports()
	serverSession, err := local.Connect(localCtx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	harness, err := mcp.NewClient(&mcp.Implementation{Name: "harness", Version: "1"}, nil).Connect(localCtx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer harness.Close()
	_, _ = harness.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
	status := func() map[string]any {
		t.Helper()
		result, err := harness.CallTool(ctx, &mcp.CallToolParams{Name: "auth_status", Arguments: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(result.StructuredContent)
		var out map[string]any
		_ = json.Unmarshal(data, &out)
		return out
	}
	diagnostics.step("test", "approval-code")
	waitFor(t, "pending approval code", func() bool { return status()["status"] == "pending" })
	pending := status()
	userCode, _ := pending["userCode"].(string)
	if userCode == "" || !strings.HasPrefix(pending["verificationUri"].(string), tls.URL+"/auth/code") {
		t.Fatalf("pending status: %v", pending)
	}

	// Browser approval against the runtime's control panel.
	csrfRE := regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	var cookie *http.Cookie
	csrf := ""
	panel := func(method, path string, values url.Values) *http.Response {
		t.Helper()
		if values == nil {
			values = url.Values{}
		}
		if method == http.MethodPost {
			values.Set("csrf", csrf)
		}
		req, _ := http.NewRequestWithContext(ctx, method, backend+path, strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	read := func(res *http.Response) string {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return string(body)
	}
	diagnostics.step("test", "browser-approval")
	page := panel(http.MethodGet, "/login", nil)
	cookie = page.Cookies()[0]
	csrf = csrfRE.FindStringSubmatch(read(page))[1]
	login := panel(http.MethodPost, "/login", url.Values{"username": {"admin"}, "password": {password}})
	read(login)
	if login.StatusCode != 303 {
		t.Fatalf("login: %d", login.StatusCode)
	}
	cookie = login.Cookies()[0]
	home := panel(http.MethodGet, "/", nil)
	csrf = csrfRE.FindStringSubmatch(read(home))[1]
	if inject && !polls.waitPending(ctx) {
		t.Fatal("no pending poll before approval")
	}
	failures := 0
	if inject {
		failures = 2
	}
	polls.arm(failures)
	approve := panel(http.MethodPost, "/auth/code", url.Values{"action": {"approve"}, "code": {userCode}, "nickname": {"agent-one"}})
	if body := read(approve); approve.StatusCode != 303 {
		t.Fatalf("approve: %d %s", approve.StatusCode, body)
	}
	diagnostics.step("test", "first-grant")
	if !polls.waitGrant(ctx) {
		t.Fatal("no first grant before test context ended")
	}
	polls.check(t)
	diagnostics.step("test", "registration")
	waitForWithin(t, 20*time.Second, "registration after approval", func() bool {
		s := status()
		return s["status"] == "authorized" && s["registered"] == true && s["name"] == "agent-one"
	})
	diagnostics.step("test", "delivery")
	sent, err := harness.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]string{"from": "agent-one", "to": "agent-one", "body": "over tls"}})
	if err != nil || sent.IsError {
		t.Fatalf("send through the session credential: %#v %v", sent, err)
	}
	select {
	case m := <-sink:
		if m.Body != "over tls" {
			t.Fatal(m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery through the credential-bearing upstream")
	}
	files, _ := filepath.Glob(filepath.Join(state.Dir(), "sessions", "*", "conn.json"))
	if len(files) != 1 {
		t.Fatalf("session state: %v", files)
	}
	if strings.Contains(read(panel(http.MethodGet, "/projects/sessions?project=devices", nil)), projectToken) {
		t.Fatal("project token shown in the panel")
	}

	// Revocation differs from explicit detach: the stored row must survive,
	// and approval of a new credential must resume that exact row.
	diagnostics.step("test", "grant-revocation")
	original := status()
	reapproval.rememberOriginal(original["registrationId"].(string), original["name"].(string))
	var saved clientstate.SessionState
	raw, err := os.ReadFile(files[0])
	if err != nil || json.Unmarshal(raw, &saved) != nil || saved.Credential == nil || saved.Credential.SessionGrantID == "" {
		t.Fatal("missing persisted grant identity")
	}
	home = panel(http.MethodGet, "/", nil)
	csrf = csrfRE.FindStringSubmatch(read(home))[1]
	revoked := panel(http.MethodPost, "/tokens/revoke", url.Values{"project": {"devices"}, "id": {saved.Credential.SessionGrantID}})
	if revoked.StatusCode != 303 {
		read(revoked)
		t.Fatal("grant revocation failed")
	}
	read(revoked)
	waitForWithin(t, 15*time.Second, "revoked grant rejection", func() bool { return status()["status"] == "rejected" })
	rejectedMessage, _ := status()["message"].(string)
	if !strings.Contains(rejectedMessage, "attach_session") || strings.Contains(rejectedMessage, "http") || strings.Contains(rejectedMessage, saved.Credential.AccessToken) {
		t.Fatal("unsafe or incomplete revoked grant guidance")
	}
	diagnostics.step("test", "replacement-approval")
	reattach, err := harness.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
	if err != nil || !reattach.IsError {
		t.Fatal("reattach did not request approval")
	}
	replacementCode, _ := status()["userCode"].(string)
	if replacementCode == "" {
		t.Fatal("no new approval code")
	}
	polls.arm(0)
	approve = panel(http.MethodPost, "/auth/code", url.Values{"action": {"approve"}, "code": {replacementCode}, "nickname": {"agent-one"}})
	if approve.StatusCode != 303 {
		read(approve)
		t.Fatal("replacement approval failed")
	}
	read(approve)
	diagnostics.step("test", "replacement-grant")
	if !polls.waitGrant(ctx) {
		t.Fatal("no replacement grant before test context ended")
	}
	polls.check(t)
	diagnostics.step("test", "reapproval-registration")
	waitForWithin(t, 20*time.Second, "same registration after reapproval", func() bool {
		current := status()
		return current["registered"] == true && current["status"] == "authorized" && current["registrationId"] == original["registrationId"] && current["name"] == original["name"]
	})
	diagnostics.step("test", "reapproval-delivery")
	sent, err = harness.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]string{"from": "agent-one", "to": "agent-one", "body": "after reapproval"}})
	if err != nil || sent.IsError {
		t.Fatal("new grant cannot send")
	}
	select {
	case m := <-sink:
		if m.Body != "after reapproval" {
			t.Fatal("wrong recovery delivery")
		}
	case <-ctx.Done():
		t.Fatal("reapproved receiver did not deliver")
	}

	// UI detach revokes the grant; the client must stop on 401 and never
	// request a new code by itself.
	diagnostics.step("test", "detach")
	registrationID, _ := status()["registrationId"].(string)
	if registrationID == "" {
		t.Fatal("registration id missing from auth_status")
	}
	sessions := panel(http.MethodGet, "/projects/sessions?project=devices", nil)
	csrf = csrfRE.FindStringSubmatch(read(sessions))[1]
	codesBefore := counting.codes.Load()
	detach := panel(http.MethodPost, "/sessions/detach", url.Values{"project": {"devices"}, "id": {registrationID}})
	if body := read(detach); detach.StatusCode != 303 {
		t.Fatalf("detach: %d %s", detach.StatusCode, body)
	}
	diagnostics.step("test", "credential-rejection")
	waitForWithin(t, 15*time.Second, "credential rejection after detach", func() bool { return status()["status"] == "rejected" })
	time.Sleep(300 * time.Millisecond)
	if counting.codes.Load() != codesBefore {
		t.Fatal("a rejected credential triggered an automatic code request")
	}
	diagnostics.step("test", "explicit-reauthorization")
	explicit, err := harness.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
	if err != nil || !explicit.IsError || counting.codes.Load() != codesBefore+1 {
		t.Fatalf("explicit re-authorization: %#v %v codes=%d", explicit, err, counting.codes.Load())
	}
	if status()["status"] != "pending" {
		t.Fatalf("after explicit attach: %v", status())
	}
	diagnostics.step("test", "client-shutdown")
	stopLocal()
	shutdown()
	diagnostics.step("test", "runtime-shutdown")
	stop()
	diagnostics.step("test", "")
}

func waitForWithin(t *testing.T, limit time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
