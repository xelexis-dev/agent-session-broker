package channel

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDirectClientRegistersWithoutDeviceApproval(t *testing.T) {
	auth := newDeviceAuthServer(t)
	original := auth.server.Config.Handler
	var exchanges atomic.Int32
	auth.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/capabilities":
			writeJSON(w, 200, map[string]any{"version": 1, "capabilities": []string{"scoped-session-v1", "direct-registration-v1"}})
		case "/oauth/session_registration":
			exchanges.Add(1)
			if r.PostFormValue("requested_nickname") != "requested-worker" {
				t.Error("configured nickname not sent")
			}
			if r.Header.Get("Authorization") == "" {
				t.Error("missing OAuth bearer")
			}
			writeJSON(w, 200, map[string]any{"access_token": fixtureToken("asb_s_"), "token_type": "Bearer", "expires_in": 3600, "session_grant_id": "synthetic-grant", "nickname": "requested-worker", "scopes": []string{"session:register", "session:read", "message:receive"}, "admission_mode": "auto"})
		default:
			original.ServeHTTP(w, r)
		}
	})
	upstream := newSwitchableUpstream(t)
	f := newDeviceModeFixture(t, auth, upstream, t.TempDir(), singleIdentity(), func(f *deviceModeFixture) {
		f.configureDurable = func(o *durableOptions) { o.device.nickname = "requested-worker" }
	})
	// The initial automatic attempt either chooses direct or starts manual flow.
	waitFor(t, "authorization attempt", func() bool { return exchanges.Load() > 0 || f.codeRequests() > 0 })
	if f.codeRequests() != 0 {
		t.Fatal("client started device flow instead of direct registration")
	}
	waitFor(t, "direct registration", func() bool { return f.status("")["registered"] == true })
	if f.status("")["name"] != "requested-worker" {
		t.Fatal("final nickname not used")
	}
}

func TestReceiveDelayedDenialOverridesTimerAllowed(t *testing.T) {
	inner := &delayedDenyUpstream{release: make(chan struct{})}
	opts := durableOptions{receive: &receiveScopes{}}
	u := opts.wrapReceive(inner, "synthetic")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = u.CallTool(context.Background(), &mcp.CallToolParams{Name: "wait_for_message"})
	}()
	v := u.(*receiveUpstream)
	select {
	case <-v.permission.decided:
	case <-time.After(4 * time.Second):
		t.Fatal("timer did not decide")
	}
	close(inner.release)
	<-done
	if !receiveDenied(u) {
		t.Fatal("authoritative denial ignored after timer allowed")
	}
}

type delayedDenyUpstream struct {
	Upstream
	release chan struct{}
}

func (u *delayedDenyUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-u.release:
		return scopeDeniedResult(), nil
	}
}

func directFixture(t *testing.T, stateDir string, handler http.HandlerFunc, configure ...func(*deviceModeFixture)) *deviceModeFixture {
	t.Helper()
	auth := newDeviceAuthServer(t)
	original := auth.server.Config.Handler
	auth.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/capabilities" {
			writeJSON(w, 200, map[string]any{"version": 1, "capabilities": []string{"scoped-session-v1", "direct-registration-v1"}})
			return
		}
		if r.URL.Path == "/oauth/session_registration" {
			handler(w, r)
			return
		}
		original.ServeHTTP(w, r)
	})
	return newDeviceModeFixture(t, auth, newSwitchableUpstream(t), stateDir, singleIdentity(), configure...)
}
func writeDirectGrant(w http.ResponseWriter, scopes []string) {
	writeJSON(w, 200, map[string]any{"access_token": fixtureToken("asb_s_"), "token_type": "Bearer", "expires_in": 3600, "session_grant_id": "synthetic-grant", "nickname": "direct-worker", "scopes": scopes, "admission_mode": "auto"})
}
func TestDirectSnapshotSkipsReceiveProbe(t *testing.T) {
	f := directFixture(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		writeDirectGrant(w, []string{"session:register", "session:read"})
	})
	waitFor(t, "registered without receive", func() bool { return f.status("")["registered"] == true })
	if f.log.last("wait_for_message") != "" {
		t.Fatal("no-receive snapshot performed a receive probe")
	}
	waitFor(t, "lease renewal", func() bool { return f.log.last("renew_lease") != "" })
	if f.log.last("wait_for_message") != "" {
		t.Fatal("renewal restarted receive")
	}
}
func TestDirectCredentialSaveFailureKeepsMCPClosed(t *testing.T) {
	f := directFixture(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) { writeDirectGrant(w, []string{"session:register"}) }, func(f *deviceModeFixture) {
		f.persist = func(s *clientstate.Store, key string, state clientstate.SessionState) error {
			if state.Credential != nil {
				return errors.New("synthetic save failure")
			}
			return s.SaveSession(key, state)
		}
	})
	waitFor(t, "save failure", func() bool { return f.status("")["status"] == authFailed })
	f.mu.Lock()
	opened := len(f.upstreams)
	f.mu.Unlock()
	if opened != 0 {
		t.Fatal("MCP opened before credential save")
	}
}
func TestDirectRequestSavedBeforeExchangeAndReusedAfterRestart(t *testing.T) {
	dir := t.TempDir()
	var firstID string
	var calls atomic.Int32
	first := directFixture(t, dir, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		files, _ := filepath.Glob(filepath.Join(dir, "sessions", "*", "conn.json"))
		if len(files) != 1 {
			t.Error("request not saved before exchange")
			return
		}
		data, _ := os.ReadFile(files[0])
		var state clientstate.SessionState
		if json.Unmarshal(data, &state) != nil || state.Direct == nil || state.Registration.Secret == "" || state.SessionID == "" {
			t.Error("request/identity/secret not durable")
			return
		}
		if state.Direct.RequestID != r.PostFormValue("request_id") {
			t.Error("wire request differs from durable request")
		}
		if firstID == "" {
			firstID = state.Direct.RequestID
		} else if firstID != state.Direct.RequestID {
			t.Error("retry changed request ID")
		}
		w.Header().Set("Retry-After", "0")
		writeJSON(w, 429, map[string]string{"error": "rate_limited"})
	})
	waitFor(t, "bounded exchange failure", func() bool { return calls.Load() >= 3 })
	first.stop()
	if calls.Load() != 3 {
		t.Fatal("rate limit retry not bounded")
	}
	first.stop()
	second := directFixture(t, dir, func(w http.ResponseWriter, r *http.Request) {
		if r.PostFormValue("request_id") != firstID {
			t.Error("restart changed request ID")
		}
		writeDirectGrant(w, []string{"session:register"})
	})
	waitFor(t, "restart direct registration", func() bool { return second.status("")["registered"] == true })
}
func TestDirectTerminalErrorsDoNotLoop(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{{401, "invalid_client"}, {403, "direct_registration_unavailable"}, {409, "identity_conflict"}, {429, "rate_limited"}} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			var n atomic.Int32
			f := directFixture(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
				n.Add(1)
				if tc.status == 429 {
					w.Header().Set("Retry-After", "3600")
				}
				writeJSON(w, tc.status, map[string]string{"error": tc.code})
			})
			waitFor(t, "terminal authorization", func() bool { return f.status("")["status"] == authFailed })
			_ = f.call("", "list_sessions", map[string]any{})
			if n.Load() != 1 || f.codeRequests() != 0 {
				t.Fatal("terminal failure retried or entered device flow")
			}
		})
	}
}
func TestDirectOrdinaryTokenFallsBackToManual(t *testing.T) {
	f := directFixture(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 403, map[string]string{"error": "insufficient_scope"})
	})
	waitFor(t, "ordinary manual flow", func() bool { return f.codeRequests() == 1 })
	if f.status("")["status"] != authPending {
		t.Fatal("permission shortage treated as credential rejection")
	}
}
func TestDirectRecoverySendsSavedProof(t *testing.T) {
	dir := t.TempDir()
	first := directFixture(t, dir, func(w http.ResponseWriter, r *http.Request) {
		writeDirectGrant(w, []string{"session:register", "session:read"})
	})
	waitFor(t, "initial saved registration", func() bool { return first.status("")["registered"] == true })
	id := first.status("")["registrationId"].(string)
	first.stop()
	files, _ := filepath.Glob(filepath.Join(dir, "sessions", "*", "conn.json"))
	data, _ := os.ReadFile(files[0])
	var state clientstate.SessionState
	if json.Unmarshal(data, &state) != nil {
		t.Fatal("state decode")
	}
	secret := state.Registration.Secret
	state.Credential = nil
	store, err := clientstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SaveSession(filepath.Base(filepath.Dir(files[0])), state); err != nil {
		t.Fatal(err)
	}
	var proof atomic.Bool
	second := directFixture(t, dir, func(w http.ResponseWriter, r *http.Request) {
		proof.Store(r.PostFormValue("registration_id") == id && r.PostFormValue("resume_secret") == secret && r.PostFormValue("request_id") != state.Direct.RequestID)
		writeDirectGrant(w, []string{"session:register"})
	})
	waitFor(t, "recovery proof exchange", func() bool { return proof.Load() })
	second.stop()
}

func TestDirectRestrictedSnapshotRejectsOldServer(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(strconv.FormatBool(scoped), func(t *testing.T) {
			auth := newDeviceAuthServer(t)
			original := auth.server.Config.Handler
			auth.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/capabilities" {
					caps := []string{}
					if scoped {
						caps = append(caps, "scoped-session-v1")
					}
					writeJSON(w, 200, map[string]any{"version": 1, "capabilities": caps})
					return
				}
				original.ServeHTTP(w, r)
			})
			store, err := clientstate.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			a := &sessionAuth{state: clientstate.SessionState{Credential: &clientstate.SessionCredential{AccessToken: "asb_s_synthetic", Scopes: []string{"session:register"}}}}
			opts := durableOptions{state: store, device: &deviceAuthOptions{authorizer: auth.client()}}
			err = opts.verifyStoredScopes(t.Context(), a)
			if scoped {
				if err != nil {
					t.Fatal("gate-off scoped server rejected")
				}
			} else if !errors.Is(err, errScopeUpgrade) {
				t.Fatal("restricted state accepted old server")
			}
		})
	}
}
func TestDirectMissingCapabilitiesUsesManual(t *testing.T) {
	for _, gateOff := range []bool{false, true} {
		t.Run(strconv.FormatBool(gateOff), func(t *testing.T) {
			auth := newDeviceAuthServer(t)
			if gateOff {
				original := auth.server.Config.Handler
				auth.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/oauth/capabilities" {
						writeJSON(w, 200, map[string]any{"version": 1, "capabilities": []string{"scoped-session-v1"}})
						return
					}
					original.ServeHTTP(w, r)
				})
			}
			f := newDeviceModeFixture(t, auth, newSwitchableUpstream(t), t.TempDir(), singleIdentity())
			waitFor(t, "manual fallback", func() bool { return f.codeRequests() == 1 })
			if f.status("")["status"] != authPending {
				t.Fatal("manual flow changed")
			}
		})
	}
}

func TestDirectRestrictedSavedCredentialCannotReachMCPOnOldServer(t *testing.T) {
	auth := newDeviceAuthServer(t) // capabilities 404
	f := newDeviceModeFixture(t, auth, newSwitchableUpstream(t), t.TempDir(), singleIdentity(), func(f *deviceModeFixture) {
		f.configureDurable = func(o *durableOptions) {
			clientID, err := o.state.ClientID()
			if err != nil {
				t.Fatal(err)
			}
			const sid = "11111111-1111-4111-8111-111111111111"
			key := clientstate.SessionKey(o.scope, "claude", sid, clientID)
			state := clientstate.SessionState{Scope: o.scope, Project: o.project, Harness: "claude", SessionID: sid, ClientID: clientID, Registration: clientstate.Registration{Secret: "synthetic-resume-secret"}, Credential: &clientstate.SessionCredential{AccessToken: "asb_s_synthetic", Scopes: []string{"session:register"}, AdmissionMode: "auto"}, Direct: &clientstate.DirectAuthorization{Restricted: true, Status: "redeemed"}}
			if err := o.state.SaveSession(key, state); err != nil {
				t.Fatal(err)
			}
		}
	})
	waitFor(t, "scope upgrade refusal", func() bool { return f.status("")["status"] == authFailed })
	_ = f.call("", "list_sessions", map[string]any{})
	f.mu.Lock()
	opened := len(f.upstreams)
	f.mu.Unlock()
	if opened != 0 || f.log.last("list_sessions") != "" {
		t.Fatal("restricted credential reached MCP after compatibility refusal")
	}
}
