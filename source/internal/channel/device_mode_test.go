package channel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
)

// recordingUpstream fronts the in-process broker for one session credential
// and records which bearer each forwarded call used. rejected simulates the
// broker answering 401 for a revoked or expired credential.
type recordingUpstream struct {
	bearer   string
	inner    Upstream
	rejected atomic.Bool
	log      *callLog
}

type callLog struct {
	mu    sync.Mutex
	calls []struct{ bearer, tool string }
}

func (l *callLog) record(bearer, tool string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, struct{ bearer, tool string }{bearer, tool})
}

func (l *callLog) last(tool string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.calls) - 1; i >= 0; i-- {
		if l.calls[i].tool == tool {
			return l.calls[i].bearer
		}
	}
	return ""
}

func (u *recordingUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	u.log.record(u.bearer, p.Name)
	if u.rejected.Load() {
		return nil, errors.New("HTTP 401")
	}
	return u.inner.CallTool(ctx, p)
}

func (u *recordingUpstream) credentialRejected() bool { return u.rejected.Load() }

// deviceModeFixture runs the local ingress server in project-token mode
// against a fake device authorization server and an in-process broker.
type deviceModeFixture struct {
	deferAttach      bool // policy tests observe a truly fresh conversation without an explicit setup request
	t                *testing.T
	auth             *deviceAuthServer
	broker           *switchableUpstream
	stateDir         string
	identities       map[string]broker.AttachRequest
	log              *callLog
	authClock        *recordingAuthClock
	mu               sync.Mutex
	upstreams        []*recordingUpstream
	grants           map[string]sessionGrant
	denied           map[string]bool
	sinks            map[string]chan broker.Message
	local            *mcp.ClientSession
	persist          func(*clientstate.Store, string, clientstate.SessionState) error
	sinkFactory      ingressSinkFactory
	sharedUpstream   Upstream
	wrapUpstream     func(string, Upstream) Upstream
	configureDurable func(*durableOptions)
	// saved records every state the client persisted, so a test can stop the
	// client only after a given state reached disk.
	saved []clientstate.SessionState
	// lossyCodes aborts that many code answers after the server recorded the
	// request, like a connection dropped before the response arrived.
	lossyCodes int
	// rejectedBearer makes every upstream built for that credential answer 401,
	// like a credential the broker revoked while the client was down.
	rejectedBearer string
	cancel         context.CancelFunc
	shutdown       func()
}

func (f *deviceModeFixture) lastSavedDeviceStatus() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.saved) == 0 || f.saved[len(f.saved)-1].Device == nil {
		return ""
	}
	return f.saved[len(f.saved)-1].Device.Status
}

func (f *deviceModeFixture) lastSavedRegistrationID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.saved) == 0 {
		return ""
	}
	return f.saved[len(f.saved)-1].Registration.RegistrationID
}

func (f *deviceModeFixture) stop() {
	f.cancel()
	f.shutdown()
}

func (f *deviceModeFixture) grant(session, credential, nickname string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grants["dev-"+session] = sessionGrant{AccessToken: credential, TokenType: "Bearer", ExpiresIn: 3600, SessionGrantID: "grant-" + nickname, Nickname: nickname}
}

func (f *deviceModeFixture) deny(session string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.denied["dev-"+session] = true
}

func (f *deviceModeFixture) codeRequests() int {
	f.auth.mu.Lock()
	defer f.auth.mu.Unlock()
	n := 0
	for _, r := range f.auth.requests {
		if r.URL.Path == "/oauth/device_authorization" {
			n++
		}
	}
	return n
}

func (f *deviceModeFixture) tokenRequests() int {
	f.auth.mu.Lock()
	defer f.auth.mu.Unlock()
	n := 0
	for _, r := range f.auth.requests {
		if r.URL.Path == "/oauth/token" {
			n++
		}
	}
	return n
}

func (f *deviceModeFixture) call(thread, tool string, args any) *mcp.CallToolResult {
	f.t.Helper()
	params := &mcp.CallToolParams{Name: tool, Arguments: args}
	if thread != "" {
		params.Meta = mcp.Meta{"threadId": thread}
	}
	result, err := f.local.CallTool(context.Background(), params)
	if err != nil {
		f.t.Fatal(tool, err)
	}
	return result
}

func (f *deviceModeFixture) status(thread string) map[string]any {
	f.t.Helper()
	result := f.call(thread, "auth_status", map[string]any{})
	data, _ := json.Marshal(result.StructuredContent)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}

func newDeviceModeFixture(t *testing.T, auth *deviceAuthServer, upstream *switchableUpstream, stateDir string, identities map[string]broker.AttachRequest, configure ...func(*deviceModeFixture)) *deviceModeFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	state, err := clientstate.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	f := &deviceModeFixture{t: t, auth: auth, broker: upstream, stateDir: stateDir, identities: identities, log: &callLog{}, grants: map[string]sessionGrant{}, denied: map[string]bool{}, sinks: map[string]chan broker.Message{}, cancel: cancel}
	for thread := range identities {
		f.sinks[thread] = make(chan broker.Message, 8)
	}
	for _, change := range configure {
		change(f)
	}
	auth.setCode(func(w http.ResponseWriter, r *http.Request) {
		session := r.PostFormValue("session_id")
		f.mu.Lock()
		lose := f.lossyCodes > 0
		if lose {
			f.lossyCodes--
		}
		f.mu.Unlock()
		if lose {
			panic(http.ErrAbortHandler)
		}
		writeJSON(w, 200, map[string]any{"device_code": "dev-" + session, "user_code": "CODE-" + session[:4], "verification_uri": "/auth/code", "expires_in": 600, "interval": 1})
	})
	auth.setToken(func(w http.ResponseWriter, r *http.Request) {
		code := r.PostFormValue("device_code")
		f.mu.Lock()
		granted, ok := f.grants[code]
		denied := f.denied[code]
		f.mu.Unlock()
		switch {
		case denied:
			writeJSON(w, 400, map[string]any{"error": "access_denied"})
		case ok:
			writeJSON(w, 200, granted)
		default:
			writeJSON(w, 400, map[string]any{"error": "authorization_pending"})
		}
	})
	authorizer := newDeviceAuthClient(auth.server.URL+"/mcp?project=brk", "asb_p_project", auth.server.Client())
	f.authClock = installRecordingAuthClock(authorizer)
	device := &deviceAuthOptions{
		authorizer: authorizer,
		clock: devicePollClock{now: time.Now, sleep: func(ctx context.Context, _ time.Duration) error {
			return realPollClock().sleep(ctx, 15*time.Millisecond)
		}},
		persist: func(s *clientstate.Store, key string, state clientstate.SessionState) error {
			f.mu.Lock()
			f.saved = append(f.saved, state)
			f.mu.Unlock()
			if f.persist != nil {
				return f.persist(s, key, state)
			}
			return s.SaveSession(key, state)
		},
		newUpstream: func(credential string) Upstream {
			u := &recordingUpstream{bearer: credential, inner: upstream, log: f.log}
			f.mu.Lock()
			u.rejected.Store(credential != "" && credential == f.rejectedBearer)
			f.upstreams = append(f.upstreams, u)
			f.mu.Unlock()
			if f.wrapUpstream != nil {
				return f.wrapUpstream(credential, u)
			}
			return u
		},
	}
	opts := durableOptions{
		enabled: true, tokenMode: true, state: state, project: "brk", scope: "https://broker/mcp?project=brk", device: device,
		observe:   func(_ context.Context, bound broker.AttachRequest) (broker.AttachRequest, error) { return bound, nil },
		heartbeat: 50 * time.Millisecond, timeout: time.Second,
		backoff: func(int) time.Duration { return 20 * time.Millisecond },
	}
	if f.configureDurable != nil {
		f.configureDurable(&opts)
	}
	identify := func(_ context.Context, req *mcp.CallToolRequest) (broker.AttachRequest, error) {
		thread := ""
		if req != nil && req.Params != nil {
			thread, _ = req.Params.Meta["threadId"].(string)
		}
		if thread == "" {
			if len(identities) == 1 {
				for _, id := range identities {
					return id, nil
				}
			}
			return broker.AttachRequest{}, errors.New("current request has no thread identity")
		}
		id, ok := identities[thread]
		if !ok {
			return broker.AttachRequest{}, errors.New("unknown thread")
		}
		return id, nil
	}
	shared := f.sharedUpstream
	if shared == nil {
		shared = unavailableUpstream{}
	}
	server, shutdown := newIngressServerWithOptions(ctx, shared, snapshotTools(), identify, func(id broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
		if f.sinkFactory != nil {
			return f.sinkFactory(id)
		}
		for thread, identity := range identities {
			if identity.SessionID == id.SessionID {
				sink := f.sinks[thread]
				return func(ctx context.Context, m broker.Message) error {
					select {
					case sink <- m:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}, nil, nil
			}
		}
		return nil, nil, errors.New("no sink for identity")
	}, opts)
	f.shutdown = shutdown
	t.Cleanup(shutdown)
	t.Cleanup(cancel)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	f.local, err = mcp.NewClient(&mcp.Implementation{Name: "harness", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.local.Close() })
	if !f.deferAttach {
		clientID, _ := state.ClientID()
		for thread, id := range identities {
			key := clientstate.SessionKey(opts.scope, id.Harness, id.SessionID, clientID)
			_, found, _ := state.LoadSession(key)
			_, legacy, _ := state.LoadRegistration(clientstate.RegistrationKey(opts.scope, id.Harness, id.SessionID))
			if !found && !legacy {
				f.call(thread, "attach_session", map[string]any{})
			}
		}
	}
	return f
}

func singleIdentity() map[string]broker.AttachRequest {
	return map[string]broker.AttachRequest{"11111111-1111-4111-8111-111111111111": {Harness: "claude", SessionID: "11111111-1111-4111-8111-111111111111", PID: 4242, CWD: `C:\work\brk`}}
}

func TestDeviceModeServesToolsBeforeApprovalAndRegistersAfter(t *testing.T) {
	auth := newDeviceAuthServer(t)
	upstream := newSwitchableUpstream(t)
	stateDir := t.TempDir()
	f := newDeviceModeFixture(t, auth, upstream, stateDir, singleIdentity())
	listed, err := f.local.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"list_sessions", "send_message", "attach_session", "auth_status"} {
		if !hasTool(listed.Tools, required) {
			t.Fatalf("tool %s missing before approval", required)
		}
	}
	waitFor(t, "device code request on initialize", func() bool { return f.codeRequests() == 1 })
	status := f.status("")
	if status["status"] != "pending" || status["userCode"] != "CODE-1111" || !strings.HasPrefix(status["verificationUri"].(string), auth.server.URL) {
		t.Fatalf("pending status: %v", status)
	}
	if result := f.call("", "list_sessions", map[string]any{}); !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "auth_status") {
		t.Fatalf("forwarded tool before approval: %#v", result)
	}
	if len(f.upstreams) != 0 {
		t.Fatal("upstream connected before a credential existed")
	}
	f.grant("11111111-1111-4111-8111-111111111111", "asb_s_one", "agent-one")
	waitFor(t, "registration after approval", func() bool {
		s, err := upstream.session("agent-one")
		return err == nil && s.AutomaticDeliveryReady
	})
	f.mu.Lock()
	bearer := f.upstreams[0].bearer
	f.mu.Unlock()
	if bearer != "asb_s_one" {
		t.Fatalf("upstream bearer: %q", bearer)
	}
	files, _ := filepath.Glob(filepath.Join(stateDir, "sessions", "*", "conn.json"))
	if len(files) != 1 {
		t.Fatalf("session state files: %v", files)
	}
	// The broker sees the registration before the client saves its answer, and
	// reading the file while the client replaces it would break the rename on
	// Windows; wait for the client's own registered signal instead.
	waitFor(t, "client reports the saved registration", func() bool { return f.status("")["registered"] == true })
	raw, _ := os.ReadFile(files[0])
	var saved clientstate.SessionState
	if err := json.Unmarshal(raw, &saved); err != nil || saved.Credential == nil || saved.Credential.AccessToken != "asb_s_one" || saved.Registration.RegistrationID == "" || saved.Device == nil || saved.Device.Status != clientstate.DeviceRedeemed {
		t.Fatalf("saved state: %s %v", raw, err)
	}
	if err := call(context.Background(), upstream, "send_message", broker.Message{From: "agent-one", To: "agent-one", Body: "approved"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-f.sinks["11111111-1111-4111-8111-111111111111"]:
		if m.Body != "approved" {
			t.Fatal(m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no delivery after approval")
	}
	status = f.status("")
	if status["status"] != "authorized" || status["name"] != "agent-one" || status["registered"] != true {
		t.Fatalf("authorized status: %v", status)
	}
	if result := f.call("", "list_sessions", map[string]any{}); result.IsError || f.log.last("list_sessions") != "asb_s_one" {
		t.Fatalf("forwarded tool after approval: %#v bearer=%q", result, f.log.last("list_sessions"))
	}
}

func TestDeviceModeTwoConversationsKeepSeparateCredentials(t *testing.T) {
	auth := newDeviceAuthServer(t)
	upstream := newSwitchableUpstream(t)
	identities := map[string]broker.AttachRequest{
		"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa": {Harness: "codex", SessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", PID: 7, CWD: `C:\work\a`},
		"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb": {Harness: "codex", SessionID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", PID: 7, CWD: `C:\work\b`},
	}
	f := newDeviceModeFixture(t, auth, upstream, t.TempDir(), identities)
	for thread := range identities {
		if status := f.status(thread); status["status"] != "pending" {
			t.Fatalf("%s: %v", thread, status)
		}
	}
	if f.codeRequests() != 2 {
		t.Fatalf("code requests: %d", f.codeRequests())
	}
	f.grant("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "asb_s_a", "agent-a")
	f.grant("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "asb_s_b", "agent-b")
	for _, name := range []string{"agent-a", "agent-b"} {
		waitFor(t, name+" registered", func() bool {
			s, err := upstream.session(name)
			return err == nil && s.AutomaticDeliveryReady
		})
	}
	if result := f.call("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "send_message", map[string]string{"from": "agent-a", "to": "agent-b", "body": "from a"}); result.IsError {
		t.Fatalf("send from a: %#v", result)
	}
	if f.log.last("send_message") != "asb_s_a" {
		t.Fatalf("send from a used bearer %q", f.log.last("send_message"))
	}
	select {
	case m := <-f.sinks["bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"]:
		if m.Body != "from a" || m.From != "agent-a" {
			t.Fatal(m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("b did not receive")
	}
	if result := f.call("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "get_session", map[string]string{"name": "agent-a"}); result.IsError || f.log.last("get_session") != "asb_s_b" {
		t.Fatalf("call from b used bearer %q", f.log.last("get_session"))
	}
	if status := f.status("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"); status["status"] != "authorized" || status["name"] != "agent-b" {
		t.Fatalf("b status: %v", status)
	}
}

func TestDeviceModeRestartRecoversPendingCredentialAndFinalAnswers(t *testing.T) {
	auth := newDeviceAuthServer(t)
	upstream := newSwitchableUpstream(t)
	stateDir := t.TempDir()
	first := newDeviceModeFixture(t, auth, upstream, stateDir, singleIdentity())
	waitFor(t, "first code request", func() bool { return first.codeRequests() == 1 })
	// Stop only after the pending code reached disk; a restart from the
	// pre-request state legitimately re-requests with the same request id.
	waitFor(t, "pending code saved", func() bool { return first.lastSavedDeviceStatus() == clientstate.DevicePending })
	first.stop()
	second := newDeviceModeFixture(t, auth, upstream, stateDir, singleIdentity())
	polls := second.tokenRequests()
	waitFor(t, "polling resumes with the saved code", func() bool { return second.tokenRequests() > polls })
	if second.codeRequests() != 1 {
		t.Fatalf("restart requested a new code: %d", second.codeRequests())
	}
	second.grant("11111111-1111-4111-8111-111111111111", "asb_s_one", "agent-one")
	waitFor(t, "registration", func() bool {
		s, err := upstream.session("agent-one")
		return err == nil && s.AutomaticDeliveryReady
	})
	registered, _ := upstream.session("agent-one")
	waitFor(t, "registration id saved", func() bool { return second.lastSavedRegistrationID() == registered.RegistrationID })
	second.stop()
	codes, tokens := second.codeRequests(), second.tokenRequests()
	third := newDeviceModeFixture(t, auth, upstream, stateDir, singleIdentity())
	waitFor(t, "resume with the saved credential", func() bool {
		s, err := upstream.session("agent-one")
		return err == nil && s.AutomaticDeliveryReady && s.Revision > registered.Revision && s.RegistrationID == registered.RegistrationID
	})
	if third.codeRequests() != codes || third.tokenRequests() != tokens {
		t.Fatal("a saved credential still touched the authorization endpoints")
	}
	third.stop()

	deniedDir := t.TempDir()
	fourth := newDeviceModeFixture(t, auth, upstream, deniedDir, map[string]broker.AttachRequest{"22222222-2222-4222-8222-222222222222": {Harness: "claude", SessionID: "22222222-2222-4222-8222-222222222222", PID: 4243, CWD: `C:\work\two`}})
	waitFor(t, "second conversation code", func() bool { return fourth.codeRequests() == codes+1 })
	fourth.deny("22222222-2222-4222-8222-222222222222")
	waitFor(t, "denied status", func() bool { return fourth.status("")["status"] == "denied" })
	waitFor(t, "denied answer saved", func() bool { return fourth.lastSavedDeviceStatus() == clientstate.DeviceDenied })
	fourth.stop()
	fifth := newDeviceModeFixture(t, auth, upstream, deniedDir, map[string]broker.AttachRequest{"22222222-2222-4222-8222-222222222222": {Harness: "claude", SessionID: "22222222-2222-4222-8222-222222222222", PID: 4243, CWD: `C:\work\two`}})
	if status := fifth.status(""); status["status"] != "denied" {
		t.Fatalf("denied answer forgotten after restart: %v", status)
	}
	time.Sleep(100 * time.Millisecond)
	if fifth.codeRequests() != codes+1 {
		t.Fatal("a final answer was followed by an automatic new code request")
	}
	if result := fifth.call("", "attach_session", map[string]any{}); !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "auth_status") {
		t.Fatalf("explicit request after denial: %#v", result)
	}
	if fifth.codeRequests() != codes+2 || fifth.status("")["status"] != "pending" {
		t.Fatal("explicit attach did not start a new authorization")
	}
}

func TestDeviceModeKeepsMCPClosedWhenCredentialCannotBeSaved(t *testing.T) {
	auth := newDeviceAuthServer(t)
	upstream := newSwitchableUpstream(t)
	f := newDeviceModeFixture(t, auth, upstream, t.TempDir(), singleIdentity(), func(f *deviceModeFixture) {
		f.persist = func(s *clientstate.Store, key string, state clientstate.SessionState) error {
			if state.Credential != nil {
				return errors.New("disk full")
			}
			return s.SaveSession(key, state)
		}
	})
	waitFor(t, "code request", func() bool { return f.codeRequests() == 1 })
	f.grant("11111111-1111-4111-8111-111111111111", "asb_s_one", "agent-one")
	waitFor(t, "save failure reported", func() bool { return f.status("")["status"] == "failed" })
	time.Sleep(100 * time.Millisecond)
	f.mu.Lock()
	connected := len(f.upstreams)
	f.mu.Unlock()
	if connected != 0 {
		t.Fatal("MCP upstream opened with an unsaved credential")
	}
	if _, err := upstream.session("agent-one"); err == nil {
		t.Fatal("registered without a saved credential")
	}
}

func TestDeviceModePublishesCredentialOnlyAfterSave(t *testing.T) {
	for _, saveSucceeds := range []bool{false, true} {
		name := "failure"
		if saveSucceeds {
			name = "success"
		}
		t.Run(name, func(t *testing.T) {
			auth := newDeviceAuthServer(t)
			upstream := newSwitchableUpstream(t)
			saving, release := make(chan struct{}), make(chan struct{})
			var started, released sync.Once
			var persisted clientstate.SessionState
			unblock := func() { released.Do(func() { close(release) }) }
			defer unblock()
			f := newDeviceModeFixture(t, auth, upstream, t.TempDir(), singleIdentity(), func(f *deviceModeFixture) {
				f.persist = func(s *clientstate.Store, key string, state clientstate.SessionState) error {
					if state.Credential != nil {
						started.Do(func() {
							persisted = state
							close(saving)
						})
						<-release
						if !saveSucceeds {
							return errors.New("disk full")
						}
					}
					return s.SaveSession(key, state)
				}
			})
			waitFor(t, "code request", func() bool { return f.codeRequests() == 1 })
			f.grant("11111111-1111-4111-8111-111111111111", "asb_s_one", "agent-one")
			select {
			case <-saving:
			case <-time.After(5 * time.Second):
				t.Fatal("credential save did not start")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			result, err := f.local.CallTool(ctx, &mcp.CallToolParams{Name: "auth_status", Arguments: map[string]any{}})
			if err != nil {
				t.Fatalf("authorization status must stay responsive while saving: %v", err)
			}
			data, _ := json.Marshal(result.StructuredContent)
			var status map[string]any
			_ = json.Unmarshal(data, &status)
			if status["status"] != "pending" {
				t.Fatalf("credential became visible before saving: %v", status)
			}
			f.mu.Lock()
			connections := len(f.upstreams)
			f.mu.Unlock()
			if connections != 0 {
				t.Fatal("MCP upstream opened before credential save completed")
			}
			unblock()
			if saveSucceeds {
				waitFor(t, "registration after save", func() bool {
					s, err := upstream.session("agent-one")
					return err == nil && s.AutomaticDeliveryReady
				})
				f.mu.Lock()
				f.upstreams[0].rejected.Store(true)
				f.mu.Unlock()
				waitFor(t, "credential rejection", func() bool { return f.status("")["status"] == "rejected" })
				if persisted.Device.Status != clientstate.DeviceRedeemed {
					t.Fatalf("live updates changed the persisted snapshot: %s", persisted.Device.Status)
				}
			} else {
				waitFor(t, "save failure", func() bool { return f.status("")["status"] == "failed" })
				f.mu.Lock()
				connections = len(f.upstreams)
				f.mu.Unlock()
				if connections != 0 {
					t.Fatal("MCP upstream opened after credential save failed")
				}
			}
		})
	}
}

func TestDeviceModeRejectedCredentialStopsWithoutNewCode(t *testing.T) {
	auth := newDeviceAuthServer(t)
	upstream := newSwitchableUpstream(t)
	f := newDeviceModeFixture(t, auth, upstream, t.TempDir(), singleIdentity())
	waitFor(t, "code request", func() bool { return f.codeRequests() == 1 })
	f.grant("11111111-1111-4111-8111-111111111111", "asb_s_one", "agent-one")
	waitFor(t, "registration", func() bool {
		s, err := upstream.session("agent-one")
		return err == nil && s.AutomaticDeliveryReady
	})
	f.mu.Lock()
	f.upstreams[0].rejected.Store(true)
	f.mu.Unlock()
	waitFor(t, "rejected status", func() bool { return f.status("")["status"] == "rejected" })
	time.Sleep(150 * time.Millisecond)
	if f.codeRequests() != 1 {
		t.Fatalf("a revoked credential triggered %d code requests", f.codeRequests())
	}
	// The fake server reuses device codes for a conversation. A fresh approval
	// request must not inherit the previous grant and race straight to authorized.
	f.mu.Lock()
	delete(f.grants, "dev-11111111-1111-4111-8111-111111111111")
	f.mu.Unlock()
	if result := f.call("", "attach_session", map[string]any{}); !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "auth_status") {
		t.Fatalf("explicit re-authorization: %#v; text: %q", result, result.Content[0].(*mcp.TextContent).Text)
	}
	if f.codeRequests() != 2 {
		t.Fatal("explicit attach did not request a new code")
	}
	if status := f.status(""); status["status"] != "pending" {
		t.Fatalf("a fresh request must wait for a new approval: %v", status)
	}
}

func TestDeviceModeReapprovalAfterRejectedSavedCredentialUsesNewCredential(t *testing.T) {
	auth := newDeviceAuthServer(t)
	upstream := newSwitchableUpstream(t)
	stateDir := t.TempDir()
	first := newDeviceModeFixture(t, auth, upstream, stateDir, singleIdentity())
	waitFor(t, "code request", func() bool { return first.codeRequests() == 1 })
	first.grant("11111111-1111-4111-8111-111111111111", "asb_s_old", "agent-one")
	waitFor(t, "registration", func() bool {
		s, err := upstream.session("agent-one")
		return err == nil && s.AutomaticDeliveryReady
	})
	registered, _ := upstream.session("agent-one")
	waitFor(t, "registration id saved", func() bool { return first.lastSavedRegistrationID() == registered.RegistrationID })
	first.stop()

	// A conversation with connection history restarts after the broker revoked
	// its saved credential: the stored credential is refused on resume.
	second := newDeviceModeFixture(t, auth, upstream, stateDir, singleIdentity(), func(f *deviceModeFixture) { f.rejectedBearer = "asb_s_old" })
	waitFor(t, "rejected status", func() bool { return second.status("")["status"] == "rejected" })
	if result := second.call("", "attach_session", map[string]any{}); !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "auth_status") {
		t.Fatalf("explicit re-authorization: %#v", result)
	}
	second.grant("11111111-1111-4111-8111-111111111111", "asb_s_new", "agent-one")
	waitFor(t, "registration with the new credential", func() bool {
		s, err := upstream.session("agent-one")
		return err == nil && s.AutomaticDeliveryReady && s.Revision > registered.Revision && second.log.last("resume_session") == "asb_s_new"
	})
	time.Sleep(150 * time.Millisecond)
	if status := second.status(""); status["status"] != "authorized" || status["registered"] != true {
		t.Fatalf("re-approved conversation: %v", status)
	}
	if bearer := second.log.last("list_sessions"); bearer != "" {
		t.Fatalf("unexpected forwarded call before the check: %q", bearer)
	}
	second.call("", "list_sessions", map[string]any{})
	if bearer := second.log.last("list_sessions"); bearer != "asb_s_new" {
		t.Fatalf("forwarded tool used credential %q after re-approval", bearer)
	}
}
