package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExplicitRegistrationProofRetryBoundary(t *testing.T) {
	for _, kind := range []string{"closed", "closed-twice", "other-error", "refusal"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			calls := 0
			u := round2CallFunc(func(got context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
				calls++
				if got != ctx || p.Name != "get_session" {
					t.Fatal("proof changed context or method")
				}
				if kind == "closed-twice" || (kind == "closed" && calls == 1) {
					return nil, fmt.Errorf("fixture: %w", mcp.ErrConnectionClosed)
				}
				if kind == "other-error" {
					return nil, fmt.Errorf("fixture transport error")
				}
				if kind == "refusal" {
					return &mcp.CallToolResult{IsError: true}, nil
				}
				return &mcp.CallToolResult{StructuredContent: broker.Session{RegistrationID: "proof-registration"}}, nil
			})
			err := explicitRegistrationProof(ctx, u, "proof-registration")
			want := 1
			if strings.HasPrefix(kind, "closed") {
				want = 2
			}
			if calls != want || (err == nil) != (kind == "closed") {
				t.Fatalf("kind=%s calls=%d want=%d success=%t", kind, calls, want, err == nil)
			}
		})
	}
}

func TestFreshConversationWaitsForExplicitAttach(t *testing.T) {
	var direct atomic.Int32
	f := directFixture(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		direct.Add(1)
		writeDirectGrant(w, []string{"session:register", "session:read", "message:receive"})
	}, func(f *deviceModeFixture) { f.deferAttach = true })
	status := f.status("")
	_ = f.call("", "list_sessions", map[string]any{})
	_ = f.call("", "ingress_hook", map[string]string{"session_id": "11111111-1111-4111-8111-111111111111", "hook_event_name": "Stop"})
	if direct.Load() != 0 || f.codeRequests() != 0 || f.tokenRequests() != 0 || f.log.count("register_session") != 0 || status["registered"] == true {
		t.Fatal("fresh conversation initiated admission or registration without explicit attach")
	}
	f.call("", "attach_session", map[string]any{})
	waitFor(t, "explicit registration", func() bool { return f.status("")["registered"] == true })
	if direct.Load() != 1 || f.log.count("register_session") != 1 {
		t.Fatal("explicit attach did not register exactly once")
	}
}

func TestFreshManualConversationAndObservedRestartDoNotRequestCode(t *testing.T) {
	dir := t.TempDir()
	shared := newSwitchableUpstream(t)
	auth := newDeviceAuthServer(t)
	for i := 0; i < 2; i++ {
		f := newDeviceModeFixture(t, auth, shared, dir, singleIdentity(), func(f *deviceModeFixture) { f.deferAttach = true })
		s := f.status("")
		_ = f.call("", "list_sessions", map[string]any{})
		_ = f.call("", "ingress_hook", map[string]string{"session_id": "11111111-1111-4111-8111-111111111111", "hook_event_name": "Stop"})
		if s["status"] != authUnauthorized || s["message"] != conversationNotRegistered || f.codeRequests() != 0 || f.tokenRequests() != 0 || f.log.count("register_session") != 0 {
			t.Fatal("diagnostics or restart initiated manual admission")
		}
		if i == 1 {
			f.call("", "attach_session", map[string]any{})
			waitFor(t, "explicit code", func() bool { return f.codeRequests() == 1 })
			if f.status("")["status"] != authPending {
				t.Fatal("explicit approval guidance lost")
			}
		}
		f.stop()
	}
}

func TestConfiguredCredentialNeedsExplicitAttachForNewIdentity(t *testing.T) {
	up := newSwitchableUpstream(t)
	up.service.SetAdmission(grantAdmission{name: "configured-worker"})
	f := newDurableFixtureWithPolicy(t, up, t.TempDir(), true)
	var s map[string]any
	if err := call(t.Context(), f.local, "auth_status", map[string]any{}, &s); err != nil {
		t.Fatal(err)
	}
	if s["status"] != authUnauthorized || s["registered"] == true {
		t.Fatal("configured credential was treated as registration")
	}
	if _, err := up.session("configured-worker"); err == nil {
		t.Fatal("configured credential registered automatically")
	}
	result, out := f.attach(t, "")
	if out.IsError || result.RegistrationID == "" {
		t.Fatal("explicit configured attach failed")
	}
	id := result.RegistrationID
	f.stop()
	next := newDurableFixtureWithPolicy(t, up, f.state.Dir(), true)
	defer next.stop()
	waitFor(t, "configured automatic resume", func() bool {
		s, err := up.session("configured-worker")
		return err == nil && s.RegistrationID == id && s.Revision > result.Revision && s.AutomaticDeliveryReady
	})
}

func TestExplicitDevicePendingContinuesAfterRestart(t *testing.T) {
	dir := t.TempDir()
	up := newSwitchableUpstream(t)
	auth := newDeviceAuthServer(t)
	first := newDeviceModeFixture(t, auth, up, dir, singleIdentity(), func(f *deviceModeFixture) { f.deferAttach = true })
	first.call("", "attach_session", map[string]any{})
	waitFor(t, "explicit pending", func() bool { return first.codeRequests() == 1 })
	_, _, before := fixtureState(t, dir)
	if before.Registration.InitialAttempt != initialPending {
		t.Fatal("initial attempt not persisted")
	}
	first.stop()
	next := newDeviceModeFixture(t, auth, up, dir, singleIdentity(), func(f *deviceModeFixture) { f.deferAttach = true })
	next.grant("11111111-1111-4111-8111-111111111111", fixtureToken("asb_s_"), "explicit-worker")
	waitFor(t, "same pending approval registers", func() bool { return next.status("")["registered"] == true })
	if next.codeRequests() != 1 || next.log.count("register_session") != 1 {
		t.Fatal("restart issued another code or duplicate registration")
	}
	_, _, after := fixtureState(t, dir)
	if after.Registration.InitialAttempt != initialComplete {
		t.Fatal("successful attempt remained active")
	}
}

func TestDeniedExpiredAndMetadataOnlyNeverAuthorizeNewRegistration(t *testing.T) {
	for _, kind := range []string{"denied", "expired", "legacy-rejected", "token-changed", "credential-only"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			up := newSwitchableUpstream(t)
			var direct atomic.Int32
			configure := func(f *deviceModeFixture) {
				f.deferAttach = true
				f.configureDurable = func(o *durableOptions) {
					cid, _ := o.state.ClientID()
					sid := "11111111-1111-4111-8111-111111111111"
					key := clientstate.SessionKey(o.scope, "claude", sid, cid)
					s := clientstate.SessionState{ClientID: cid, Scope: o.scope, Project: o.project, Harness: "claude", SessionID: sid, LastAdmissionMode: "auto", AutoRecovery: &clientstate.AutoRecovery{Kind: "token-changed"}, Registration: clientstate.Registration{Secret: "synthetic-secret", Name: "cached-nickname"}}
					switch kind {
					case "denied":
						s.Registration.InitialAttempt = initialStopped
						s.Device = &clientstate.DeviceAuthorization{Status: clientstate.DeviceDenied}
					case "expired":
						s.Registration.InitialAttempt = initialPending
						s.Device = &clientstate.DeviceAuthorization{Status: clientstate.DevicePending, ExpiresAt: time.Now().Add(-time.Minute), RequestID: "expired-code-request"}
					case "legacy-rejected":
						s.Device = &clientstate.DeviceAuthorization{Status: clientstate.DeviceRejected}
					case "token-changed":
						s.AdmissionFingerprint = "old-source"
						s.Device = &clientstate.DeviceAuthorization{Status: clientstate.DeviceRejected}
					case "credential-only":
						s.Credential = &clientstate.SessionCredential{AccessToken: fixtureToken("asb_s_"), AdmissionMode: "auto"}
					}
					if e := o.state.SaveSession(key, s); e != nil {
						t.Fatal(e)
					}
				}
			}
			f := recoveryFixture(t, dir, up, func(w http.ResponseWriter, r *http.Request) { direct.Add(1); recoveryGrant(w, 1) }, configure)
			for i := 0; i < 2; i++ {
				_ = f.status("")
				_ = f.call("", "list_sessions", map[string]any{})
			}
			if direct.Load() != 0 || f.codeRequests() != 0 || f.tokenRequests() != 0 || f.log.count("register_session") != 0 {
				t.Fatal("metadata or terminal attempt authorized initial registration")
			}
			f.stop()
			// Re-open the same state without reseeding it or providing a new request.
			next := recoveryFixture(t, dir, up, func(w http.ResponseWriter, r *http.Request) { direct.Add(1); recoveryGrant(w, 2) }, func(f *deviceModeFixture) { f.deferAttach = true })
			_ = next.status("")
			if direct.Load() != 0 || next.codeRequests() != 0 || next.log.count("register_session") != 0 {
				t.Fatal("restart restarted a terminal/nonexistent attempt")
			}
		})
	}
}

func TestInitialAttemptCompatibilityInference(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		state clientstate.SessionState
		want  bool
	}{
		{"plain credential", clientstate.SessionState{Credential: &clientstate.SessionCredential{}}, false},
		{"device pending", clientstate.SessionState{Device: &clientstate.DeviceAuthorization{Status: clientstate.DevicePending, ExpiresAt: now.Add(time.Hour)}}, true},
		{"direct pending", clientstate.SessionState{Direct: &clientstate.DirectAuthorization{Status: "requested", CreatedAt: now}}, true},
		{"redeemed credential", clientstate.SessionState{Direct: &clientstate.DirectAuthorization{Status: "redeemed"}, Credential: &clientstate.SessionCredential{}}, true},
		{"register replay", clientstate.SessionState{Registration: clientstate.Registration{PendingRequestID: "same-request", PendingLease: "same-lease"}}, true},
		{"stopped wins", clientstate.SessionState{Registration: clientstate.Registration{InitialAttempt: initialStopped, PendingRequestID: "old-request", PendingLease: "old-lease"}, Direct: &clientstate.DirectAuthorization{Status: "requested", CreatedAt: now}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.state.Registration.Secret = "synthetic-secret"
			inferInitialAttempt(&tc.state, now)
			if registrationPermitted(tc.state.Registration) != tc.want {
				t.Fatal("incorrect compatibility continuation permission")
			}
		})
	}
}

type pausedInitialRegister struct {
	Upstream
	started chan struct{}
}

func (u *pausedInitialRegister) credentialRejected() bool { return upstreamRejected(u.Upstream) }
func (u *pausedInitialRegister) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name == "register_session" {
		select {
		case u.started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return u.Upstream.CallTool(ctx, p)
}

func TestExplicitApprovedAndLostRegisterContinueSameAttemptAfterRestart(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint("applied-response-lost=", lost), func(t *testing.T) {
			dir := t.TempDir()
			up := newSwitchableUpstream(t)
			var grants atomic.Int32
			started := make(chan struct{}, 1)
			if lost {
				up.loseAtDeadline.Store("register_session")
				up.appliedBeforeAnswerLoss = make(chan struct{})
			}
			first := recoveryFixture(t, dir, up, func(w http.ResponseWriter, r *http.Request) { recoveryGrant(w, grants.Add(1)) }, func(f *deviceModeFixture) {
				f.deferAttach = true
				if !lost {
					f.wrapUpstream = func(_ string, u Upstream) Upstream { return &pausedInitialRegister{Upstream: u, started: started} }
				}
			})
			returned := make(chan struct{})
			go func() {
				defer close(returned)
				_, _ = first.local.CallTool(t.Context(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
			}()
			signal := (<-chan struct{})(started)
			if lost {
				signal = up.appliedBeforeAnswerLoss
			}
			select {
			case <-signal:
			case <-time.After(3 * time.Second):
				t.Fatal("register phase did not start")
			}
			_, _, before := fixtureState(t, dir)
			if before.Credential == nil || before.Registration.InitialAttempt != initialPending || before.Registration.PendingRequestID == "" {
				t.Fatal("approval/register continuation not durable")
			}
			first.stop()
			select {
			case <-returned:
			case <-time.After(time.Second):
				t.Fatal("explicit call survived shutdown")
			}
			var replay broker.RegisterRequest
			next := recoveryFixture(t, dir, up, func(w http.ResponseWriter, r *http.Request) { grants.Add(1); recoveryGrant(w, 99) }, func(f *deviceModeFixture) {
				f.deferAttach = true
				f.wrapUpstream = func(_ string, u Upstream) Upstream {
					return &captureRegister{Upstream: u, capture: func(r broker.RegisterRequest) { replay = r }}
				}
			})
			waitFor(t, "registered same attempt", func() bool { return next.status("")["registered"] == true })
			if grants.Load() != 1 || next.codeRequests() != 0 || replay.RequestID != before.Registration.PendingRequestID || replay.ChannelID != before.Registration.PendingLease || replay.ExpectedRevision != before.Registration.PendingRevision {
				t.Fatal("restart changed grant/request/lease/revision")
			}
		})
	}
}

type captureRegister struct {
	Upstream
	capture func(broker.RegisterRequest)
}

func (u *captureRegister) credentialRejected() bool { return upstreamRejected(u.Upstream) }
func (u *captureRegister) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name == "register_session" {
		data, _ := json.Marshal(p.Arguments)
		var r broker.RegisterRequest
		_ = json.Unmarshal(data, &r)
		u.capture(r)
	}
	return u.Upstream.CallTool(ctx, p)
}

func TestFreshClaudeActivationPreservesUnregisteredAndPendingGuidance(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	auth := newDeviceAuthServer(t)
	up := newSwitchableUpstream(t)
	store, _ := clientstate.Open(t.TempDir())
	log := &callLog{}
	id := singleIdentity()["11111111-1111-4111-8111-111111111111"]
	opts := durableOptions{enabled: true, tokenMode: true, state: store, scope: "https://broker/mcp?project=brk", project: "brk", observe: func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return id, nil }, device: &deviceAuthOptions{authorizer: auth.client(), clock: devicePollClock{now: time.Now, sleep: func(ctx context.Context, _ time.Duration) error {
		return realPollClock().sleep(ctx, 20*time.Millisecond)
	}}, newUpstream: func(token string) Upstream { return &recordingUpstream{bearer: token, inner: up, log: log} }}}
	st, ct := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- ServeDurableChannel(ctx, st, unavailableUpstream{}, snapshotTools(), id, opts) }()
	conn, e := ct.Connect(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("channel did not stop")
		}
	}()
	write := func(raw string) {
		m, e := jsonrpc.DecodeMessage([]byte(raw))
		if e != nil {
			t.Fatal(e)
		}
		if e = conn.Write(ctx, m); e != nil {
			t.Fatal(e)
		}
	}
	read := func() []byte {
		m, e := conn.Read(ctx)
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := jsonrpc.EncodeMessage(m)
		return raw
	}
	write(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	raw := read()
	if !strings.Contains(string(raw), "Do not call attach_session merely") {
		t.Fatal("explicit registration instructions missing")
	}
	write(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	var event struct {
		Params struct {
			Meta map[string]string `json:"meta"`
		} `json:"params"`
	}
	if json.Unmarshal(read(), &event) != nil || event.Params.Meta["activation"] == "" {
		t.Fatal("missing activation token")
	}
	activation, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "activate_channel", "arguments": map[string]string{"token": event.Params.Meta["activation"]}}})
	write(string(activation))
	_ = read()
	write(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"auth_status","arguments":{}}}`)
	if !strings.Contains(string(read()), conversationNotRegistered) {
		t.Fatal("activation replaced unregistered guidance")
	}
	auth.mu.Lock()
	requests := len(auth.requests)
	auth.mu.Unlock()
	if requests != 0 || log.count("register_session") != 0 {
		t.Fatal("fresh activation initiated registration")
	}
	write(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"attach_session","arguments":{}}}`)
	_ = read()
	write(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"auth_status","arguments":{}}}`)
	if !strings.Contains(string(read()), "approval-pending") {
		t.Fatal("pending approval guidance lost after explicit request")
	}
}
