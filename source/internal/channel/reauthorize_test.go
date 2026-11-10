package channel

import (
	"context"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAutoReauthorizationAfterReceiverRejection(t *testing.T) {
	var exchanges atomic.Int32
	f := directFixture(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		n := exchanges.Add(1)
		if n > 1 && (r.PostFormValue("registration_id") == "" || r.PostFormValue("resume_secret") == "" || r.PostFormValue("requested_nickname") != "") {
			t.Error("recovery must send saved proof and omit requested nickname")
		}
		writeJSON(w, 200, map[string]any{"access_token": fmt.Sprintf("asb_s_synthetic-%d", n), "token_type": "Bearer", "expires_in": 3600, "session_grant_id": "synthetic-grant", "nickname": "direct-worker", "scopes": []string{"session:register", "session:read", "message:receive"}, "admission_mode": "auto"})
	})
	waitFor(t, "initial registration", func() bool { return f.status("")["registered"] == true })
	original, err := f.broker.session("direct-worker")
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.upstreams[0].rejected.Store(true)
	f.mu.Unlock()
	// No tools/call until automatic recovery has completed.
	waitForWithin(t, 3*time.Second, "automatic exchange without tools/call", func() bool { return exchanges.Load() == 2 })
	waitFor(t, "same registration resumed", func() bool {
		current, err := f.broker.session("direct-worker")
		return err == nil && current.RegistrationID == original.RegistrationID && current.Generation > original.Generation && current.AutomaticDeliveryReady
	})
	waitFor(t, "client confirms resumed registration", func() bool {
		s := f.status("")
		return s["registered"] == true && s["name"] == "direct-worker" && s["registrationId"] == original.RegistrationID
	})
	if f.codeRequests() != 0 || exchanges.Load() != 2 {
		t.Fatal("recovery entered device flow or repeated exchange")
	}
}

type recoveryUpstream struct {
	Upstream
	resumeCode      string
	active, maximum *atomic.Int32
}

func (u *recoveryUpstream) credentialRejected() bool { return upstreamRejected(u.Upstream) }
func (u *recoveryUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name == "resume_session" && u.resumeCode != "" {
		return &mcp.CallToolResult{StructuredContent: broker.RegisterResult{Code: u.resumeCode}}, nil
	}
	if p.Name == "wait_for_message" && u.active != nil {
		n := u.active.Add(1)
		defer u.active.Add(-1)
		for previous := u.maximum.Load(); n > previous; previous = u.maximum.Load() {
			if u.maximum.CompareAndSwap(previous, n) {
				break
			}
		}
	}
	return u.Upstream.CallTool(ctx, p)
}

func recoveryFixture(t *testing.T, dir string, shared *switchableUpstream, handler http.HandlerFunc, configure ...func(*deviceModeFixture)) *deviceModeFixture {
	t.Helper()
	auth := newDeviceAuthServer(t)
	original := auth.server.Config.Handler
	auth.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/capabilities":
			writeJSON(w, 200, map[string]any{"version": 1, "capabilities": []string{"scoped-session-v1", "direct-registration-v1"}})
		case "/oauth/session_registration":
			handler(w, r)
		default:
			original.ServeHTTP(w, r)
		}
	})
	if shared == nil {
		shared = newSwitchableUpstream(t)
	}
	return newDeviceModeFixture(t, auth, shared, dir, singleIdentity(), configure...)
}
func recoveryGrant(w http.ResponseWriter, n int32) {
	writeJSON(w, 200, map[string]any{"access_token": fmt.Sprintf("asb_s_synthetic-%d", n), "token_type": "Bearer", "expires_in": 3600, "session_grant_id": "synthetic-grant", "nickname": "direct-worker", "scopes": []string{"session:register", "session:read", "message:receive"}, "admission_mode": "auto"})
}
func rejectFirst(f *deviceModeFixture) {
	f.mu.Lock()
	f.upstreams[0].rejected.Store(true)
	f.mu.Unlock()
}
func (l *callLog) count(tool string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.calls {
		if c.tool == tool {
			n++
		}
	}
	return n
}

func TestAutoRecoveryConcurrentToolsHaveOneExchangeAndReceiver(t *testing.T) {
	var n, active, maximum atomic.Int32
	exchanging, release := make(chan struct{}), make(chan struct{})
	f := recoveryFixture(t, t.TempDir(), nil, func(w http.ResponseWriter, r *http.Request) {
		next := n.Add(1)
		if next == 2 {
			close(exchanging)
			<-release
		}
		recoveryGrant(w, next)
	}, func(f *deviceModeFixture) {
		f.wrapUpstream = func(_ string, u Upstream) Upstream {
			return &recoveryUpstream{Upstream: u, active: &active, maximum: &maximum}
		}
	})
	waitFor(t, "initial", func() bool { return f.status("")["registered"] == true })
	rejectFirst(f)
	select {
	case <-exchanging:
	case <-time.After(3 * time.Second):
		t.Fatal("no automatic exchange")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = f.call("", "list_sessions", map[string]any{}) }()
	}
	wg.Wait()
	s := f.status("")
	if s["registered"] == true || !strings.Contains(s["message"].(string), "automatic reauthorization is in progress") {
		t.Fatal("recovery readiness/guidance")
	}
	close(release)
	waitFor(t, "client recovery", func() bool { return f.status("")["registered"] == true })
	if n.Load() != 2 || maximum.Load() != 1 || f.log.count("register_session") != 1 {
		t.Fatal("overlapping exchange/receiver or new registration")
	}
}

func TestAutoRecoveryTerminalExchangeErrorsDoNotFallbackOrReset(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{{401, "invalid_client"}, {403, "insufficient_scope"}, {403, "policy"}, {404, "unsupported"}, {409, "identity_conflict"}, {400, "invalid_request"}, {200, "malformed"}} {
		t.Run(fmt.Sprint(tc.status, tc.code), func(t *testing.T) {
			var n atomic.Int32
			f := recoveryFixture(t, t.TempDir(), nil, func(w http.ResponseWriter, r *http.Request) {
				if n.Add(1) == 1 {
					recoveryGrant(w, 1)
					return
				}
				writeJSON(w, tc.status, map[string]string{"error": tc.code})
			})
			waitFor(t, "initial", func() bool { return f.status("")["registered"] == true })
			rejectFirst(f)
			waitFor(t, "stopped", func() bool {
				s := f.status("")
				m, _ := s["message"].(string)
				return strings.Contains(m, "automatic-reauthorization-stopped")
			})
			for i := 0; i < 3; i++ {
				_ = f.call("", "list_sessions", map[string]any{})
			}
			if n.Load() != 2 || f.codeRequests() != 0 || f.log.count("register_session") != 1 || f.status("")["registered"] == true {
				t.Fatal("terminal recovery retried, fell back, or claimed readiness")
			}
			if tc.status == 401 && !strings.Contains(f.status("")["message"].(string), "Replace the project token") {
				t.Fatal("missing token replacement step")
			}
		})
	}
}

func TestAutoRecoveryResumeTerminalKeepsUnregistered(t *testing.T) {
	for _, code := range []string{broker.CodeNotFound, broker.CodeDetached, broker.CodeOwned, broker.CodeInvalid} {
		t.Run(code, func(t *testing.T) {
			var n atomic.Int32
			f := recoveryFixture(t, t.TempDir(), nil, func(w http.ResponseWriter, r *http.Request) { recoveryGrant(w, n.Add(1)) }, func(f *deviceModeFixture) {
				f.wrapUpstream = func(token string, u Upstream) Upstream {
					c := ""
					if token == "asb_s_synthetic-2" {
						c = code
					}
					return &recoveryUpstream{Upstream: u, resumeCode: c}
				}
			})
			waitFor(t, "initial", func() bool { return f.status("")["registered"] == true })
			rejectFirst(f)
			waitFor(t, "resume stopped", func() bool {
				m, _ := f.status("")["message"].(string)
				return strings.Contains(m, "registration-"+code)
			})
			if f.status("")["registered"] == true || f.log.count("register_session") != 1 || f.codeRequests() != 0 {
				t.Fatal("resume failure claimed readiness or created registration")
			}
		})
	}
}

func TestAutoRecoveryRejectedStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	shared := newSwitchableUpstream(t)
	var n atomic.Int32
	first := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) { recoveryGrant(w, n.Add(1)) })
	waitFor(t, "initial", func() bool { return first.status("")["registered"] == true })
	id := first.status("")["registrationId"]
	first.stop()
	store, _ := clientstate.Open(dir)
	cid, _ := store.ClientID()
	key := clientstate.SessionKey("https://broker/mcp?project=brk", "claude", "11111111-1111-4111-8111-111111111111", cid)
	state, _, err := store.LoadSession(key)
	if err != nil {
		t.Fatal(err)
	}
	opts := durableOptions{state: store, device: &deviceAuthOptions{}}
	a := &sessionAuth{key: key, state: state}
	opts.rejectCredentialIfCurrent(a, state.Credential.AccessToken)
	saved, _, _ := store.LoadSession(key)
	if saved.LastAdmissionMode != "auto" || saved.Credential != nil {
		t.Fatal("auto evidence lost")
	}
	second := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) { recoveryGrant(w, n.Add(1)) })
	waitFor(t, "restart recovery", func() bool { s := second.status(""); return s["registered"] == true && s["registrationId"] == id })
	if n.Load() != 2 || second.log.count("register_session") != 0 {
		t.Fatal("restart did not resume")
	}
}
