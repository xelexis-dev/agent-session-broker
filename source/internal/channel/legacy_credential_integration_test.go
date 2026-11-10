package channel

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverruntime"
)

// Real SQLite policy, HTTP authorization, broker registration, local MCP and
// persisted resume state. Only harness observation and delivery are substituted.
func TestLegacyRevokedTokenReplacementResumesSameRegistration(t *testing.T) {
	diagnostics := newTLSDiagnostics()
	diagnostics.scope = "legacy-client-http"
	diagnostics.step("legacy", "setup")
	t.Cleanup(func() { diagnostics.report(t.Failed(), t.Logf) })
	transport := newFixtureDiagnosticTransport(diagnostics)
	t.Cleanup(transport.next.CloseIdleConnections)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	root := t.TempDir()
	policyDir := filepath.Join(root, "control")
	store, err := controlpanel.OpenSQLite(policyDir)
	if err != nil {
		t.Fatal(err)
	}
	const password = "integration-fixture-password"
	if err = store.Setup(password); err != nil {
		t.Fatal(err)
	}
	if err = store.SaveProject("legacy", "token", true, ""); err != nil {
		t.Fatal(err)
	}
	if err = store.SetInstanceHTTPPolicy("admin", true); err != nil {
		t.Fatal(err)
	}
	token, err := store.IssueToken("legacy", "legacy-one")
	if err != nil {
		t.Fatal(err)
	}
	tokenID := store.Projects()[0].Tokens[0].ID
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	reservation.Close()
	base := "http://" + address
	runtimeStarts := 0
	startRuntime := func() func() {
		runtimeStarts++
		phase := "first-runtime"
		if runtimeStarts > 1 {
			phase = "replacement-runtime"
		}
		diagnostics.step("runtime", phase)
		serverCtx, stop := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- serverruntime.Run(serverCtx, []string{"--listen", address, "--memory-dir", root}, "test", io.Discard)
		}()
		running := true
		shutdown := func() {
			if !running {
				return
			}
			running = false
			diagnostics.step("runtime", phase+"-shutdown")
			before := diagnostics.snapshot()
			// Canceled MCP requests can finish an unclaimed dial after their
			// receiver has stopped. Close this fixture's idle pool before the
			// runtime's 5s Shutdown sees a fresh socket with no HTTP headers.
			transport.next.CloseIdleConnections()
			stop()
			select {
			case err := <-done:
				if err != nil {
					t.Logf("legacy-runtime-before-stop %s", before)
					diagnostics.report(true, t.Logf)
					t.Errorf("runtime stop: %v", err)
				}
			case <-time.After(7 * time.Second):
				t.Logf("legacy-runtime-before-stop %s", before)
				diagnostics.report(true, t.Logf)
				t.Error("runtime stop timeout")
			}
		}
		t.Cleanup(shutdown)
		waitFor(t, "runtime health", func() bool {
			res, err := (&http.Client{Transport: transport, Timeout: time.Second}).Get(base + "/healthz")
			if err != nil {
				return false
			}
			res.Body.Close()
			return res.StatusCode == 200
		})
		return shutdown
	}
	stopRuntime := startRuntime()
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	endpoint := base + "/mcp?project=legacy"
	identity := broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111112281", PID: 2281, CWD: t.TempDir()}
	connect := func(bearer string) (*mcp.ClientSession, *brokerUpstream, func()) {
		localCtx, stop := context.WithCancel(ctx)
		up := newTestBrokerUpstream(localCtx, endpoint)
		up.transport = transport
		up.setBearerToken(bearer)
		opts := durableOptions{enabled: true, tokenMode: true, state: state, scope: brokerScope(endpoint, "legacy"), project: "legacy", heartbeat: 20 * time.Millisecond, timeout: time.Second, backoff: func(int) time.Duration { return time.Millisecond }}
		opts.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return identity, nil }
		server, shutdown := newIngressServerWithOptions(localCtx, up, snapshotTools(), func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) { return identity, nil }, func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
			return func(context.Context, broker.Message) error { return nil }, nil, nil
		}, opts)
		st, ct := mcp.NewInMemoryTransports()
		ss, err := server.Connect(localCtx, st, nil)
		if err != nil {
			t.Fatal(err)
		}
		local, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(localCtx, ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		running := true
		close := func() {
			if !running {
				return
			}
			running = false
			stop()
			shutdown()
			local.Close()
			ss.Close()
			up.Close()
		}
		t.Cleanup(close)
		return local, up, close
	}
	local, up, closeLocal := connect(token)
	_, _ = local.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
	var original broker.Session
	waitFor(t, "legacy registration", func() bool {
		return call(ctx, up, "get_session", map[string]string{"name": "legacy-one"}, &original) == nil && original.AutomaticDeliveryReady
	})
	if original.RegistrationID == "" {
		t.Fatal("no registration ID")
	}
	// Revoke through the real authenticated panel while the runtime is live.
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Transport: transport, Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	csrfRE := regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	getCSRF := func(path string) string {
		res, err := browser.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		match := csrfRE.FindSubmatch(body)
		if len(match) != 2 {
			t.Fatal("missing csrf")
		}
		return string(match[1])
	}
	post := func(path string, values url.Values) {
		res, err := browser.PostForm(base+path, values)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != 303 {
			t.Fatalf("panel mutation returned %d", res.StatusCode)
		}
	}
	csrf := getCSRF("/login")
	post("/login", url.Values{"csrf": {csrf}, "username": {"admin"}, "password": {password}})
	csrf = getCSRF("/")
	post("/tokens/revoke", url.Values{"csrf": {csrf}, "project": {"legacy"}, "id": {tokenID}})
	waitFor(t, "legacy 401", func() bool { _ = call(ctx, up, "list_sessions", struct{}{}, nil); return up.credentialRejected() })
	var status authStatus
	if err := call(ctx, local, "auth_status", struct{}{}, &status); err != nil {
		t.Fatal(err)
	}
	if status.Status != "rejected" || !strings.Contains(status.Message, "replace") || !strings.Contains(status.Message, "reconnect") {
		t.Fatal("legacy recovery guidance missing")
	}
	result, err := local.CallTool(ctx, &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})

	var rejection string
	if err != nil {
		rejection = err.Error()
	} else {
		if !result.IsError {
			t.Fatal("revoked legacy call succeeded")
		}
		data, _ := json.Marshal(result)
		rejection = string(data)
	}
	if !strings.Contains(rejection, "configured session credential") || strings.Contains(rejection, token) || strings.Contains(rejection, base) {
		t.Fatal("unsafe or incomplete forwarded rejection")
	}

	diagnostics.step("legacy", "first-client-close")
	closeLocal()
	stopRuntime()
	// User replaces a revoked token while keeping this client's state directory.
	store, err = controlpanel.OpenSQLite(policyDir)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := store.IssueToken("legacy", "legacy-one")
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	stopRuntime = startRuntime()
	nextLocal, nextUp, closeNext := connect(replacement)
	if err := call(ctx, nextLocal, "attach_session", map[string]any{}, nil); err != nil {
		t.Fatal("replacement token could not resume the saved registration")
	}
	var resumed broker.Session
	waitFor(t, "replacement resume", func() bool {
		return call(ctx, nextUp, "get_session", map[string]string{"name": "legacy-one"}, &resumed) == nil && resumed.AutomaticDeliveryReady && resumed.Revision > original.Revision
	})
	if resumed.RegistrationID != original.RegistrationID || resumed.Name != original.Name {
		t.Fatal("legacy replacement lost registration identity")
	}
	diagnostics.step("legacy", "replacement-client-close")
	closeNext()
	stopRuntime()
}
