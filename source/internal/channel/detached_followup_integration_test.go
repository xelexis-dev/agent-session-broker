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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverruntime"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

type detachedHarness struct {
	received chan broker.Message
	client   *mcp.ClientSession
	close    func()
	session  broker.Session
}
type detachedRequest struct {
	kind, session, registration string
}

type detachedRuntime struct {
	requestMu             sync.Mutex
	shortCalls            map[chan struct{}]*http.Request
	attachmentGate        atomic.Pointer[func(http.ResponseWriter, *http.Request) bool]
	requestCompleted      atomic.Pointer[func(*http.Request, int)]
	renews                atomic.Int32
	requests              []detachedRequest
	otherToken            string
	dropHandover          atomic.Bool
	root, endpoint, token string
	manualToken           string
	direct, resumes       atomic.Int32
	ctx                   context.Context
}

func (r *detachedRuntime) recordRequest(kind, session, registration string) {
	r.requestMu.Lock()
	defer r.requestMu.Unlock()
	r.requests = append(r.requests, detachedRequest{kind, session, registration})
}
func (r *detachedRuntime) requestSnapshot() []detachedRequest {
	r.requestMu.Lock()
	defer r.requestMu.Unlock()
	return append([]detachedRequest(nil), r.requests...)
}

// Gate choice and request registration share a lock so installation can snapshot every earlier short call.
func (r *detachedRuntime) beginShortRequest(req *http.Request) (*func(http.ResponseWriter, *http.Request) bool, func()) {
	short := strings.HasPrefix(req.URL.Path, "/oauth/")
	if req.URL.Path == "/mcp" && req.Method == "POST" {
		data, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(strings.NewReader(string(data)))
		var rpc struct{ Params struct{ Name string } }
		_ = json.Unmarshal(data, &rpc)
		switch rpc.Params.Name {
		case "register_session", "resume_session", "renew_lease", "probe_response", "report_ingress_failure":
			short = true
		}
	}
	r.requestMu.Lock()
	gate := r.attachmentGate.Load()
	var done chan struct{}
	if short {
		done = make(chan struct{})
		if r.shortCalls == nil {
			r.shortCalls = map[chan struct{}]*http.Request{}
		}
		r.shortCalls[done] = req
	}
	r.requestMu.Unlock()
	return gate, func() {
		if done != nil {
			r.requestMu.Lock()
			delete(r.shortCalls, done)
			close(done)
			r.requestMu.Unlock()
		}
	}
}

func (r *detachedRuntime) installGate(gate *func(http.ResponseWriter, *http.Request) bool) map[*http.Request]<-chan struct{} {
	r.requestMu.Lock()
	defer r.requestMu.Unlock()
	r.attachmentGate.Store(gate)
	pending := make(map[*http.Request]<-chan struct{}, len(r.shortCalls))
	for done, req := range r.shortCalls {
		pending[req] = done
	}
	return pending
}

func newDetachedRuntime(t *testing.T, attachmentsEnabled ...bool) *detachedRuntime {
	enabled := len(attachmentsEnabled) > 0 && attachmentsEnabled[0]
	return newDetachedRuntimeBinary(t, enabled, "")
}
func newDetachedRuntimeBinary(t *testing.T, enabled bool, binary string) *detachedRuntime {
	t.Helper()
	ctx := t.Context()
	root := t.TempDir()
	s, err := controlpanel.OpenSQLite(filepath.Join(root, "control"))
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []func() error{func() error { return s.Setup("synthetic-detached-password") }, func() error { return s.CreateProject("detach") }, func() error { return s.SetScopeIssuanceReady("admin", true) }, func() error { return s.SetInstanceHTTPPolicy("admin", true) }} {
		if err = op(); err != nil {
			t.Fatal(err)
		}
	}
	token, err := s.IssueProjectTokenWithOptionsFor("admin", "detach", "detached-test", controlpanel.ProjectTokenOptions{Scopes: append(access.LegacyV1Scopes(), access.SessionRegisterAuto)})
	if err != nil {
		t.Fatal(err)
	}
	manualToken, e := s.IssueProjectTokenWithOptionsFor("admin", "detach", "ordinary-test", controlpanel.ProjectTokenOptions{Scopes: access.LegacyV1Scopes()})
	if e != nil {
		t.Fatal(e)
	}
	if err = s.CreateProject("other"); err != nil {
		t.Fatal(err)
	}
	otherToken, e := s.IssueProjectTokenWithOptionsFor("admin", "other", "other-auto", controlpanel.ProjectTokenOptions{Scopes: append(access.LegacyV1Scopes(), access.SessionRegisterAuto)})
	if e != nil {
		t.Fatal(e)
	}
	if enabled {
		if err = s.SetAttachmentPolicy("admin", true); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	port, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := port.Addr().String()
	port.Close()
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		if binary != "" {
			cmd := exec.CommandContext(runCtx, binary, "--listen", address, "--memory-dir", root)
			cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
			e := cmd.Run()
			if runCtx.Err() != nil {
				e = nil
			}
			done <- e
		} else {
			done <- serverruntime.Run(runCtx, []string{"--listen", address, "--memory-dir", root}, "test", io.Discard)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(7 * time.Second):
			t.Error("temporary detached runtime did not stop")
		}
	})
	health := &http.Client{Timeout: time.Second}
	origin := "http://" + address
	waitFor(t, "temporary detached runtime", func() bool {
		r, e := health.Get(origin + "/healthz")
		if e != nil {
			return false
		}
		r.Body.Close()
		return r.StatusCode == 200
	})
	out := &detachedRuntime{root: root, token: token, otherToken: otherToken, manualToken: manualToken, ctx: ctx}
	destination, _ := url.Parse(origin)
	reverse := httputil.NewSingleHostReverseProxy(destination)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gate, finish := out.beginShortRequest(r)
		defer finish()
		if completed := out.requestCompleted.Load(); completed != nil {
			response := &detachResponseStatus{ResponseWriter: w, status: http.StatusOK}
			w = response
			defer func() { (*completed)(r, response.status) }()
		}
		if gate != nil && (*gate)(w, r) {
			return
		}
		if r.URL.Path == "/oauth/session_registration" {
			out.direct.Add(1)
			data, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(data)))
			form, _ := url.ParseQuery(string(data))
			out.recordRequest("admission", form.Get("session_id"), form.Get("registration_id"))
			if out.dropHandover.Load() {
				if form.Get("handover") == "true" && out.dropHandover.Swap(false) {
					recorder := httptest.NewRecorder()
					reverse.ServeHTTP(recorder, r)
					panic(http.ErrAbortHandler)
				}
			}
		}
		if r.URL.Path == "/mcp" && r.Method == "POST" {
			data, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(data)))
			var rpc struct {
				Params struct {
					Name      string
					Arguments struct {
						SessionID      string `json:"sessionId"`
						RegistrationID string `json:"registrationId"`
					}
				}
			}
			_ = json.Unmarshal(data, &rpc)
			if rpc.Params.Name == "renew_lease" {
				out.renews.Add(1)
			}
			if rpc.Params.Name == "resume_session" {
				out.resumes.Add(1)
				out.recordRequest("resume", rpc.Params.Arguments.SessionID, rpc.Params.Arguments.RegistrationID)
			}
		}
		reverse.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	out.endpoint = proxy.URL + "/mcp?project=detach"
	return out
}
func (r *detachedRuntime) harness(t *testing.T, stateDir string, n int, configure ...func(*durableOptions)) *detachedHarness {
	id := broker.AttachRequest{Harness: "codex", SessionID: fmt.Sprintf("11111111-1111-4111-8111-%012d", n), PID: n, CWD: "/synthetic/detached"}
	return r.identityHarness(t, stateDir, id, configure...)
}
func (r *detachedRuntime) identityHarness(t *testing.T, stateDir string, id broker.AttachRequest, configure ...func(*durableOptions)) *detachedHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(r.ctx)
	state, err := clientstate.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	opts := durableOptions{receiverIdentity: fixtureReceiverIdentity{}, enabled: true, tokenMode: true, state: state, project: "detach", scope: brokerScope(r.endpoint, "detach"), heartbeat: 30 * time.Millisecond, timeout: time.Second, backoff: func(int) time.Duration { return 10 * time.Millisecond }, observe: func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return id, nil }}
	opts.device = &deviceAuthOptions{authorizer: newDeviceAuthClient(r.endpoint, r.token, nil), clock: realPollClock(), newUpstream: func(token string) Upstream {
		u := newTestBrokerUpstream(ctx, r.endpoint)
		u.setBearerToken(token)
		return u
	}}
	for _, apply := range configure {
		apply(&opts)
	}
	messages := make(chan broker.Message, 32)
	identify := func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) { return id, nil }
	if opts.claudeStartupClock.now != nil {
		identify = claudeConversationIdentity(id, &opts)
	}
	factory := ingressSinkFactory(func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
		return func(ctx context.Context, m broker.Message) error {
			select {
			case messages <- m:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}, nil, nil
	})
	if opts.claudeInboxFactory != nil {
		factory = opts.claudeInboxFactory
	}
	local, shutdown := newIngressServerWithOptions(ctx, unavailableUpstream{}, snapshotTools(), identify, factory, opts)
	st, ct := mcp.NewInMemoryTransports()
	server, err := local.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := mcp.NewClient(&mcp.Implementation{Name: "synthetic-detach-harness", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	var closed atomic.Bool
	out := &detachedHarness{received: messages, client: client, close: func() {
		if closed.Swap(true) {
			return
		}
		cancel()
		client.Close()
		server.Close()
		shutdown()
	}}
	t.Cleanup(out.close)
	return out
}
func (h *detachedHarness) attach(t *testing.T) {
	t.Helper()
	if err := call(t.Context(), h.client, "attach_session", map[string]any{}, &h.session); err != nil {
		t.Fatal("explicit attach", err)
	}
}
func (h *detachedHarness) status(t *testing.T) authStatus {
	t.Helper()
	var out authStatus
	if err := call(t.Context(), h.client, "auth_status", map[string]any{}, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func (r *detachedRuntime) denials(t *testing.T) int {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(r.root, "control"), "control.db")
	if err != nil {
		t.Fatal("audit evidence open", err)
	}
	defer db.Close()
	rows, err := db.List()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for key, raw := range rows {
		if strings.HasPrefix(key, "audit/") {
			var e controlpanel.AuditEvent
			if err := json.Unmarshal(raw, &e); err != nil {
				t.Fatal(err)
			}
			if e.Kind == "rejected" {
				n++
			}
		}
	}
	return n
}

func TestDetachedRealRuntimeRestartWaitsForExplicitAttach(t *testing.T) {
	r := newDetachedRuntime(t)
	manager := r.harness(t, t.TempDir(), 901)
	manager.attach(t)
	state := t.TempDir()
	first := r.harness(t, state, 902)
	first.attach(t)
	original := first.session.RegistrationID
	if err := call(t.Context(), manager.client, "stop_session", map[string]string{"name": original}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "detach observed before shutdown", func() bool { return strings.Contains(first.status(t).Message, "registration-detached") })
	first.close()
	before := r.direct.Load()
	denials := r.denials(t)
	next := r.harness(t, state, 902)
	waitFor(t, "restart observed detached", func() bool { return strings.Contains(next.status(t).Message, "registration-detached") })
	// Repeated diagnostics/ordinary tools exercise automatic attachment after the
	// definitive detached response. This is client work, never a model attach.
	for i := 0; i < 12; i++ {
		_ = next.status(t)
		_, _ = next.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
		time.Sleep(25 * time.Millisecond)
	}
	if r.direct.Load() != before || r.denials(t) != denials {
		t.Fatalf("automatic readmission after detach: exchanges=%d denials=%d", r.direct.Load()-before, r.denials(t)-denials)
	}
	next.attach(t)
	if next.session.RegistrationID != original {
		t.Fatal("explicit revival changed registration ID")
	}
}
func TestDetachedRealRuntimeStatusDoesNotClaimProgress(t *testing.T) {
	for _, self := range []bool{true, false} {
		t.Run(fmt.Sprint("self=", self), func(t *testing.T) {
			r := newDetachedRuntime(t)
			manager := r.harness(t, t.TempDir(), 911)
			manager.attach(t)
			target := r.harness(t, t.TempDir(), 912)
			target.attach(t)
			caller := manager
			if self {
				caller = target
			}
			if err := call(t.Context(), caller.client, "stop_session", map[string]string{"name": target.session.RegistrationID}, nil); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "detached receiver retired", func() bool { return !target.status(t).Registered })
			out := target.status(t)
			if out.Status != authAuthorized || out.Registered || strings.Contains(strings.ToLower(out.Message), "in progress") || !strings.Contains(out.Message, "attach_session") {
				t.Fatalf("detached status misleading: status=%s registered=%v message=%s", out.Status, out.Registered, out.Message)
			}
		})
	}
}

// A detached registration remains detached even when the stored credential is
// subsequently rejected. Older clients forgot detach on restart and attempted
// direct reauthorization against a registration the server cannot admit.
func TestDetachedRealRuntimeRejectedCredentialDoesNotReadmit(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	first := r.harness(t, dir, 921)
	first.attach(t)
	if err := call(t.Context(), first.client, "stop_session", map[string]string{"name": first.session.RegistrationID}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "detach observed", func() bool { return strings.Contains(first.status(t).Message, "registration-detached") })
	first.close()
	files, err := filepath.Glob(filepath.Join(dir, "sessions", "*", "conn.json"))
	if err != nil || len(files) != 1 {
		t.Fatal("expected one saved conversation")
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var saved clientstate.SessionState
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Registration.Detached {
		t.Fatal("detached observation was not persisted")
	}
	saved.Credential = nil
	saved.Device = &clientstate.DeviceAuthorization{Status: clientstate.DeviceRejected, RequestID: "synthetic-rejected-request"}
	store, err := clientstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SaveSession(filepath.Base(filepath.Dir(files[0])), saved); err != nil {
		t.Fatal(err)
	}
	before, denials := r.direct.Load(), r.denials(t)
	next := r.harness(t, dir, 921)
	for i := 0; i < 12; i++ {
		_ = next.status(t)
		_, _ = next.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
		time.Sleep(25 * time.Millisecond)
	}
	if r.direct.Load() != before || r.denials(t) != denials {
		t.Fatalf("known detached credential rejection caused direct=%d denials=%d", r.direct.Load()-before, r.denials(t)-denials)
	}
}
