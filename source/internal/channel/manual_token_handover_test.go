package channel

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

func manualApproval(t *testing.T, r *detachedRuntime, code, name string) {
	t.Helper()
	origin := strings.Split(r.endpoint, "/mcp")[0]
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	csrfPattern := regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	request := func(method, path string, values url.Values) (int, string) {
		t.Helper()
		var body io.Reader
		if values != nil {
			body = strings.NewReader(values.Encode())
		}
		req, _ := http.NewRequestWithContext(t.Context(), method, origin+path, body)
		if values != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal("synthetic approval HTTP request failed")
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(data)
	}
	_, page := request("GET", "/login", nil)
	match := csrfPattern.FindStringSubmatch(page)
	if len(match) != 2 {
		t.Fatal("synthetic login CSRF missing")
	}
	status, _ := request("POST", "/login", url.Values{"username": {"admin"}, "password": {"synthetic-detached-password"}, "csrf": {match[1]}})
	if status != 303 {
		t.Fatal("synthetic login failed")
	}
	_, page = request("GET", "/", nil)
	match = csrfPattern.FindStringSubmatch(page)
	if len(match) != 2 {
		t.Fatal("synthetic approval CSRF missing")
	}
	status, _ = request("POST", "/auth/code", url.Values{"action": {"approve"}, "code": {code}, "nickname": {name}, "csrf": {match[1]}})
	if status != 303 {
		t.Fatalf("synthetic device approval failed: HTTP %d", status)
	}
}
func manualAttachApprove(t *testing.T, r *detachedRuntime, h *detachedHarness, name string, args map[string]any, approved ...func()) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	out, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "attach_session", Arguments: args})
	if err != nil {
		t.Fatal("manual attach transport failed")
	}
	if !out.IsError {
		t.Fatal("manual attach unexpectedly skipped approval")
	}
	var code string
	waitForWithin(t, 3*time.Second, "manual approval pending", func() bool { code = h.status(t).UserCode; return code != "" })
	manualApproval(t, r, code, name)
	for _, release := range approved {
		release()
	}
	waitForWithin(t, 15*time.Second, "registration after manual approval", func() bool { return h.status(t).Registered })
	h.attach(t)
}
func TestManualTokenHandoverPreservesRegistrationQueueAndCredential(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	initial := claudeHandoverIdentity(901, 11101)
	var oldApproval, nextApproval *scheduledManualAuthorizer
	old := r.identityHarness(t, dir, initial, func(o *durableOptions) {
		handoverOptions(o)
		oldApproval = scheduledManualApproval(o, r)
		upstream := o.device.newUpstream
		o.device.newUpstream = func(token string) Upstream { return &handoverBlockedReceiver{upstream(token).(*brokerUpstream)} }
	})
	manualAttachApprove(t, r, old, "manual-original", nil, oldApproval.approved)
	_, _, saved := handoverSaved(t, r, dir, initial)
	manager := r.harness(t, t.TempDir(), 11102)
	manager.attach(t)
	if err := call(t.Context(), manager.client, "send_message", broker.Message{From: manager.session.Name, To: old.session.Name, Body: "manual-queued"}, nil); err != nil {
		t.Fatal("manual queue setup failed")
	}
	old.close()
	next := r.identityHarness(t, dir, claudeHandoverIdentity(902, 11101), func(o *durableOptions) {
		handoverOptions(o)
		nextApproval = scheduledManualApproval(o, r)
	})
	manualAttachApprove(t, r, next, "manual-original-next", nil, nextApproval.approved)
	if next.session.RegistrationID != old.session.RegistrationID || next.session.Name != old.session.Name {
		t.Fatal("manual approval created a fresh registration instead of rebinding the original")
	}
	select {
	case message := <-next.received:
		if message.Body != "manual-queued" {
			t.Fatal("wrong queued envelope")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("manual rebind lost queued message")
	}
	probe := newTestBrokerUpstream(t.Context(), r.endpoint)
	probe.setBearerToken(saved.Credential.AccessToken)
	defer probe.Close()
	if err := call(t.Context(), probe, "list_sessions", map[string]any{}, nil); err == nil {
		t.Fatal("old conversation credential was not revoked")
	}
	db, err := sqlitestore.Open(filepath.Join(r.root, "control"), "control.db")
	if err != nil {
		t.Fatal("audit open failed")
	}
	defer db.Close()
	rows, err := db.List()
	if err != nil {
		t.Fatal("audit list failed")
	}
	found := false
	for key, data := range rows {
		if strings.HasPrefix(key, "audit/") {
			var event struct {
				Kind     string
				Handover *broker.HandoverAudit
			}
			if json.Unmarshal(data, &event) == nil && event.Kind == "handover" && event.Handover != nil && event.Handover.RegistrationID == old.session.RegistrationID {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("manual handover audit missing")
	}
}

type scheduledManualAuthorizer struct {
	*deviceAuthClient
	mu    sync.Mutex
	ready chan struct{}
}

func (a *scheduledManualAuthorizer) requestCode(ctx context.Context, r deviceCodeRequest) (deviceAuthorization, error) {
	a.mu.Lock()
	a.ready = make(chan struct{})
	a.mu.Unlock()
	return a.deviceAuthClient.requestCode(ctx, r)
}
func (a *scheduledManualAuthorizer) approved() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ready != nil {
		select {
		case <-a.ready:
		default:
			close(a.ready)
		}
	}
}
func (a *scheduledManualAuthorizer) redeem(ctx context.Context, code string) (sessionGrant, tokenOutcome, error) {
	a.mu.Lock()
	ready := a.ready
	a.mu.Unlock()
	select {
	case <-ready:
		return a.deviceAuthClient.redeem(ctx, code)
	default:
		return sessionGrant{}, tokenPending, nil
	}
}
func (a *scheduledManualAuthorizer) sleep(ctx context.Context, _ time.Duration) error {
	a.mu.Lock()
	ready := a.ready
	a.mu.Unlock()
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func scheduledManualApproval(o *durableOptions, r *detachedRuntime) *scheduledManualAuthorizer {
	a := &scheduledManualAuthorizer{deviceAuthClient: newDeviceAuthClient(r.endpoint, r.manualToken, nil)}
	o.device.authorizer = a
	o.device.clock = devicePollClock{now: time.Now, sleep: a.sleep}
	return a
}

type countingManualAuthorizer struct {
	deviceAuthorizer
	requests *atomic.Int32
}

func (a *countingManualAuthorizer) requestCode(ctx context.Context, r deviceCodeRequest) (deviceAuthorization, error) {
	a.requests.Add(1)
	return a.deviceAuthorizer.requestCode(ctx, r)
}
func TestManualHandoverLiveConversationRequiresApprovalAndReturnDoesNotRequestDevice(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	initial := claudeHandoverIdentity(910, 11201)
	current := claudeHandoverIdentity(911, initial.PID)
	roster := &followRoster{id: initial}
	var requests atomic.Int32
	var original *ingressBinding
	var approval *scheduledManualAuthorizer
	h := r.identityHarness(t, dir, initial, func(o *durableOptions) {
		followOptions(roster)(o)
		approval = scheduledManualApproval(o, r)
		o.device.authorizer = &countingManualAuthorizer{approval, &requests}
		o.bindingCreated = func(b *ingressBinding) {
			if b.identity.SessionID == initial.SessionID {
				original = b
			}
		}
	})
	manualAttachApprove(t, r, h, "manual-live", nil, approval.approved)
	before := h.session
	manager := r.harness(t, t.TempDir(), 11202)
	manager.attach(t)
	roster.set(current)
	for i := 0; i < 3; i++ {
		status := h.status(t)
		if !strings.Contains(status.Message, "follow-skipped: approval-required") {
			t.Fatal("manual follow did not require approval")
		}
	}
	if requests.Load() != 1 {
		t.Fatal("automatic manual follow requested device approval")
	}
	if err := call(t.Context(), manager.client, "send_message", broker.Message{From: manager.session.Name, To: before.Name, Body: "manual-live-pending"}, nil); err != nil {
		t.Fatal("pending setup failed")
	}
	waitFor(t, "manual source delivery pause", func() bool {
		original.mu.Lock()
		defer original.mu.Unlock()
		return original.followStopped && original.followMessage != nil
	})
	manualAttachApprove(t, r, h, "manual-live-next", nil, approval.approved)
	if h.session.ID != current.SessionID || h.session.RegistrationID != before.RegistrationID || h.session.Name != before.Name {
		t.Fatal("same-client explicit approval failed to publish the new binding")
	}
	select {
	case message := <-h.received:
		if message.Body != "manual-live-pending" {
			t.Fatal("wrong pending message")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("manual approval did not transfer client-stashed envelope")
	}
	if requests.Load() != 2 {
		t.Fatal("manual handover requested more than one new approval")
	}
	h.close()
	var returnRequests atomic.Int32
	returned := r.identityHarness(t, dir, initial, func(o *durableOptions) {
		handoverOptions(o)
		o.device.authorizer = &countingManualAuthorizer{newDeviceAuthClient(r.endpoint, r.manualToken, nil), &returnRequests}
	})
	status := returned.status(t)
	if status.Registered || status.Diagnostics == nil || status.Diagnostics.HandoverSkipped != "approval-required" || returnRequests.Load() != 0 {
		t.Fatal("automatic marker return did not stop before device approval")
	}
}

type missingRebindUpstream struct{ Upstream }

func (u missingRebindUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name == "rebind_session" {
		return nil, errors.New("unknown tool: rebind_session")
	}
	return u.Upstream.CallTool(ctx, p)
}
func (u missingRebindUpstream) Close() error { closeUpstream(u.Upstream); return nil }
func TestManualTakeoverAndOldServerNeverCreateFreshRegistration(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "takeover", true: "old-server"}[legacy], func(t *testing.T) {
			r := newDetachedRuntime(t)
			dir := t.TempDir()
			initial := claudeHandoverIdentity(920, 11301)
			var source *ingressBinding
			var oldApproval, nextApproval *scheduledManualAuthorizer
			old := r.identityHarness(t, dir, initial, func(o *durableOptions) {
				handoverOptions(o)
				o.bindingCreated = func(b *ingressBinding) { source = b }
				oldApproval = scheduledManualApproval(o, r)
			})
			manualAttachApprove(t, r, old, "manual-takeover", nil, oldApproval.approved)
			before := old.session
			_, _, saved := handoverSaved(t, r, dir, initial)
			old.close()
			source.mu.Lock()
			lease := source.identity.ChannelID
			source.mu.Unlock()
			probe := newTestBrokerUpstream(t.Context(), r.endpoint)
			probe.setBearerToken(saved.Credential.AccessToken)
			if err := call(t.Context(), probe, "report_ingress_failure", map[string]string{"registrationId": before.RegistrationID, "channelId": lease}, nil); err != nil {
				t.Fatal("cannot mark closed source ingress unavailable")
			}
			probe.Close()
			next := r.identityHarness(t, dir, claudeHandoverIdentity(921, 11302), func(o *durableOptions) {
				handoverOptions(o)
				nextApproval = scheduledManualApproval(o, r)
				if legacy {
					upstream := o.device.newUpstream
					o.device.newUpstream = func(token string) Upstream { return missingRebindUpstream{upstream(token)} }
				}
			})
			if !legacy {
				manualAttachApprove(t, r, next, "manual-takeover-next", map[string]any{"takeover": before.Name}, nextApproval.approved)
				if next.session.RegistrationID != before.RegistrationID || next.session.Name != before.Name {
					t.Fatal("named manual takeover created a fresh registration")
				}
				return
			}
			result, err := next.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{"takeover": before.Name}})
			if err != nil || !result.IsError {
				t.Fatal("old-server manual takeover did not request approval")
			}
			status := next.status(t)
			if status.UserCode == "" {
				t.Fatal("manual code missing")
			}
			manualApproval(t, r, status.UserCode, "manual-takeover-next")
			nextApproval.approved()
			waitForWithin(t, 15*time.Second, "old-server refusal", func() bool {
				status = next.status(t)
				return status.Diagnostics != nil && status.Diagnostics.HandoverSkipped == "unsupported-mode"
			})
			if status.Registered {
				t.Fatal("old server created a fresh registration")
			}
			_, _, saved = handoverSaved(t, r, dir, claudeHandoverIdentity(921, 11302))
			if saved.Handover == nil || saved.Handover.StopReason != "unsupported-mode" || saved.Registration.RegistrationID != "" {
				t.Fatal("old-server refusal did not remain terminal for automatic registration")
			}
			// No bearer-only self-release tool exists for an unregistered manual grant;
			// the unused temporary-name credential follows the normal unused expiry.
		})
	}
}
func TestManualUnknownTakeoverDoesNotRequestApproval(t *testing.T) {
	r := newDetachedRuntime(t)
	var requests atomic.Int32
	h := r.identityHarness(t, t.TempDir(), claudeHandoverIdentity(930, 11401), func(o *durableOptions) {
		handoverOptions(o)
		o.device.authorizer = &countingManualAuthorizer{newDeviceAuthClient(r.endpoint, r.manualToken, nil), &requests}
	})
	handoverRefusal(t, h, "unknown-local-name", "not-found")
	if requests.Load() != 0 {
		t.Fatal("unknown local takeover requested device approval")
	}
	for _, tool := range snapshotTools() {
		if tool.Name == "rebind_session" {
			t.Fatal("rebind exposed in forwarded snapshot")
		}
	}
}

func TestManualDeviceAuthorizationBypassesOnlyCompletionRetryGate(t *testing.T) {
	for _, test := range []struct {
		name, status, device string
		credential, allowed  bool
	}{
		{"pending", authPending, clientstate.DevicePending, false, true},
		{"authorized", authAuthorized, clientstate.DeviceRedeemed, true, true},
		{"no-device", authAuthorized, "", true, false},
		{"denied", authDenied, clientstate.DeviceDenied, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := &sessionAuth{status: test.status}
			a.state.Registration.InitialAttempt = initialPending
			if test.device != "" {
				a.state.Device = &clientstate.DeviceAuthorization{Status: test.device}
			}
			if test.credential {
				a.state.Credential = &clientstate.SessionCredential{AdmissionMode: "manual"}
			}
			b := &ingressBinding{mu: &sync.Mutex{}, durable: &durableRegistration{session: a}}
			options := (durableOptions{}).withDefaults()
			f := &claudeFollow{opts: &options, retryID: "target", retryAt: time.Now().Add(time.Hour)}
			if f.completionRetryBlocked("target", b) == test.allowed {
				t.Fatal("explicit completion and automatic retry were not distinguished")
			}
			a.state.Registration.InitialAttempt = initialStopped
			if !f.completionRetryBlocked("target", b) {
				t.Fatal("completed or stopped authorization bypassed retry gate")
			}
		})
	}
}

func TestManualFollowDoesNotRearmApprovalWhileDeviceRequestCompletes(t *testing.T) {
	for _, status := range []string{authPending, authAuthorized} {
		t.Run(status, func(t *testing.T) {
			f, o, initial, _ := fastFollowFixture(t)
			current := claudeHandoverIdentity(999, initial.PID)
			source := f.lookup(initial.SessionID)
			source.durable.session.state.LastAdmissionMode = "manual"
			a := &sessionAuth{status: status}
			a.state.Registration.InitialAttempt = initialPending
			a.state.Device = &clientstate.DeviceAuthorization{Status: clientstate.DevicePending}
			if status == authAuthorized {
				a.state.Device.Status = clientstate.DeviceRedeemed
				a.state.Credential = &clientstate.SessionCredential{AdmissionMode: "manual"}
			}
			target := &ingressBinding{mu: &sync.Mutex{}, durable: &durableRegistration{session: a}}
			f.lookup = func(id string) *ingressBinding {
				if id == initial.SessionID {
					return source
				}
				if id == current.SessionID {
					return target
				}
				return nil
			}
			o.claudeSessionFile = func(int) ([]byte, error) { return nil, errors.New("synthetic file unavailable") }
			o.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return current, nil }
			f.retryID = current.SessionID
			f.retryAt = time.Now().Add(-time.Minute)
			before := f.retryAt
			f.result = "prior diagnostic"
			f.move = func(context.Context, broker.AttachRequest, broker.AttachRequest) (broker.AttachRequest, error) {
				t.Fatal("approval completion attempted automatic handover")
				return initial, nil
			}
			for i := 0; i < 3; i++ {
				got, err := f.check(t.Context(), initial)
				if err != nil || got.SessionID != initial.SessionID {
					t.Fatal("pending approval changed live binding")
				}
			}
			if !f.retryAt.Equal(before) || f.result != "prior diagnostic" || source.receiveReason != "" {
				t.Fatal("periodic check re-armed refusal or repeated diagnostic")
			}
		})
	}
}
