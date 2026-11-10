package channel

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"os"
	"path/filepath"
)

func TestMisuseBootstrapTransientOutageRetries(t *testing.T) {
	central := recoveryBroker(t)
	var offline atomic.Bool
	offline.Store(true)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			http.Error(w, "synthetic outage", 503)
			return
		}
		central.Config.Handler.ServeHTTP(w, r)
	}))
	defer gateway.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	u := newTestBrokerUpstream(ctx, gateway.URL+"/mcp?project=brk")
	defer u.Close()
	retries := 0
	tools, err := initialBrokerTools(ctx, u, func(int) time.Duration { retries++; offline.Store(false); return 0 })
	if err != nil || len(tools) == 0 || retries != 1 {
		t.Fatalf("startup did not recover a temporary server outage: connected=%v retries=%d", err == nil, retries)
	}
}

func TestMisuseB1TwoClaudeClientsKeepOneLocalReceiver(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	id := claudeHandoverIdentity(1501, 21501)
	first := r.identityHarness(t, dir, id, handoverOptions)
	first.attach(t)
	before := first.session
	secondID := id
	secondID.PID++
	second := r.identityHarness(t, dir, secondID, handoverOptions)
	for i := 0; i < 3; i++ {
		s := second.status(t)
		if s.Registered || s.ReceiveError != "owned-local" || s.Message != localClientLockMessage {
			t.Fatal("second process did not report local ownership")
		}
	}
	manager := r.harness(t, t.TempDir(), 21503)
	manager.attach(t)
	if err := call(t.Context(), manager.client, "send_message", broker.Message{From: manager.session.Name, To: before.Name, Body: "duplicate-client-test"}, nil); err != nil {
		t.Fatal("message enqueue failed")
	}
	select {
	case <-first.received:
	case <-time.After(2 * time.Second):
		t.Fatal("owner did not receive")
	}
	select {
	case <-second.received:
		t.Fatal("blocked client received a message")
	default:
	}
	select {
	case <-first.received:
		t.Fatal("owner received twice")
	default:
	}
	current := first.status(t)
	if !current.Registered || current.RegistrationID != before.RegistrationID {
		t.Fatal("blocked process stole the registration")
	}
	second.close()
	first.close()
	replacement := r.identityHarness(t, dir, secondID, handoverOptions)
	waitFor(t, "replacement resume after owner exit", func() bool {
		s := replacement.status(t)
		return s.Registered && s.RegistrationID == before.RegistrationID
	})
}

// Copied state bypasses the local OS lock, but cannot replace a live receiver.
func TestMisuseB1CopiedProofCannotReconcileLiveOtherReceiver(t *testing.T) {
	u := newSwitchableUpstream(t)
	id := claudeHandoverIdentity(1510, 21510)
	secret := clientstate.NewSecret()
	owner := fixtureHolder()
	owner.PID = 101
	req := broker.RegisterRequest{Holder: owner, Harness: "claude", SessionID: id.SessionID, ClientID: "copied-client", PID: id.PID, CWD: id.CWD, Name: "copied-owner", Secret: secret, RequestID: "original-request", ChannelID: "original-lease"}
	var registered broker.RegisterResult
	if err := call(t.Context(), u, "register_session", req, &registered); err != nil || registered.Code != "" {
		t.Fatal("fixture register failed")
	}
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal("fixture state")
	}
	challenger := *owner
	challenger.PID = 102
	d := &durableRegistration{key: clientstate.RegistrationKey("copied", "claude", id.SessionID), clientID: req.ClientID, registrationID: registered.Session.RegistrationID, name: req.Name, secret: secret, revision: registered.Session.Revision}
	b := &ingressBinding{mu: &sync.Mutex{}, identity: id, durable: d}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waits := 0
	opts := (durableOptions{state: state, scope: "copied", project: "brk", receiverIdentity: fixtureReceiverIdentity{holder: &challenger}, resumeClock: devicePollClock{sleep: func(ctx context.Context, delay time.Duration) error {
		waits++
		if delay != time.Second {
			t.Fatal("wrong local holder check interval")
		}
		cancel()
		return ctx.Err()
	}}}).withDefaults()
	if _, ok := resumeDurable(ctx, u, b, opts, ""); ok || waits != 1 || !b.holderWaiting {
		t.Fatal("copied proof did not wait for live holder")
	}
	if d.registrationID != registered.Session.RegistrationID || d.secret != secret {
		t.Fatal("waiting replaced registration proof")
	}
	var renewed broker.RegisterResult
	if err := call(t.Context(), u, "renew_lease", broker.RenewRequest{RegistrationID: d.registrationID, ChannelID: req.ChannelID}, &renewed); err != nil || renewed.Code != "" {
		t.Fatal("challenger fenced original holder")
	}
}

func TestMisuseB5UnsupportedParentHasNoFallback(t *testing.T) {
	for _, classify := range []func(int) (string, error){func(int) (string, error) { return "", nil }, func(int) (string, error) { return "", errors.New("synthetic process unavailable") }} {
		if _, _, err := nearestAgent([]int{321, 123}, classify); err == nil {
			t.Fatal("unsupported parent accepted")
		}
	}
	id, err := nearestClaudeIdentity(t.Context(), 10, func(context.Context) ([]rosterRow, error) {
		return []rosterRow{{SessionID: claudeHandoverIdentity(1520, 20).SessionID, PID: 20, Kind: "interactive"}}, nil
	}, func(context.Context, string) (broker.AttachRequest, error) {
		t.Fatal("inspected unrelated parent")
		return broker.AttachRequest{}, nil
	}, time.Millisecond, time.Millisecond)
	if err == nil || id.SessionID != "" {
		t.Fatal("unrelated Claude ancestor accepted")
	}
}

func TestMisuseB8URLAndFlagErrorsAreSafe(t *testing.T) {
	for _, endpoint := range []string{"", "ftp://broker/mcp?project=p", "https://broker/mcp/?project=p", "https://broker/other?project=p", "https://broker/mcp", "https://broker/mcp?project=a&project=b", "https://user:synthetic-private@broker/mcp?project=p", "https://broker/mcp?project=p#synthetic-private"} {
		_, err := parseClientOptions([]string{"--broker-url", endpoint, "--token", "synthetic-private"})
		if err == nil || strings.Contains(err.Error(), "synthetic-private") || strings.Contains(err.Error(), endpoint) && endpoint != "" {
			t.Fatal("invalid URL accepted or echoed")
		}
	}
	_, err := parseClientOptions([]string{"--unknown=synthetic-private"})
	if err == nil || strings.Contains(err.Error(), "synthetic-private") {
		t.Fatal("flag parser leaked caller input")
	}
}

func TestMisuseB7FiveClearAndReturnKeepOneRegistration(t *testing.T) {
	r := newDetachedRuntime(t)
	original := claudeHandoverIdentity(1530, 21530)
	roster := &followRoster{id: original}
	h := r.identityHarness(t, t.TempDir(), original, followOptions(roster))
	h.attach(t)
	saved := h.session
	admissions := r.direct.Load()
	for i := 1; i <= 5; i++ {
		current := claudeHandoverIdentity(1530+i, original.PID)
		roster.set(current)
		waitFor(t, "clear follow", func() bool {
			s := h.status(t)
			return s.Registered && s.RegistrationID == saved.RegistrationID && strings.Contains(s.Message, "followed: handover")
		})
	}
	roster.set(original)
	waitFor(t, "return to original", func() bool { return h.status(t).Registered })
	s := h.status(t)
	if s.RegistrationID != saved.RegistrationID || s.Name != saved.Name {
		t.Fatal("burst or return created another registration")
	}
	other := original
	other.SessionID = claudeHandoverIdentity(1599, original.PID).SessionID
	other.CWD = "/synthetic/other-project"
	before := r.direct.Load()
	roster.set(other)
	s = h.status(t)
	if r.direct.Load() != before || !strings.Contains(s.Message, "follow-skipped: parent-changed") {
		t.Fatal("cross-cwd follow was not refused")
	}
	if admissions == 0 {
		t.Fatal("fixture did not register")
	}
}

func TestMisuseB2CorruptStartupReportsSafeCause(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	store, err := clientstate.Open(dir)
	if err != nil {
		t.Fatal("state open failed")
	}
	cid, err := store.ClientID()
	if err != nil {
		t.Fatal("client id setup failed")
	}
	id := claudeHandoverIdentity(1540, 21540)
	key := clientstate.SessionKey(brokerScope(r.endpoint, "detach"), "claude", id.SessionID, cid)
	if err := store.SaveSession(key, clientstate.SessionState{Registration: clientstate.Registration{Secret: clientstate.NewSecret()}}); err != nil {
		t.Fatal("state setup failed")
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions", key, "conn.json"), []byte("{truncated-synthetic-private"), 0600); err != nil {
		t.Fatal("corruption injection failed")
	}
	// Startup pruning takes conn.lock even for fresh records before checking
	// age. Finish that independent maintenance first; this test diagnoses the
	// corrupt record, not the scheduling of the startup worker.
	if _, err := store.Prune(time.Now()); err != nil {
		t.Fatal("fixture maintenance failed")
	}
	release, err := store.LockSession(key)
	if err != nil {
		t.Fatal("fixture maintenance lock failed")
	}
	var releaseOnce sync.Once
	unlock := func() { releaseOnce.Do(release) }
	defer unlock()
	h := r.identityHarness(t, dir, id, handoverOptions)
	// Reproduce the original ordering deterministically: a maintenance-held
	// lock legitimately reports owned-local before the corrupt file is read.
	locked := h.status(t)
	if locked.ReceiveError != "owned-local" || locked.Registered {
		t.Fatal("maintenance lock did not precede state loading")
	}
	unlock()
	result, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "auth_status", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal("diagnostic transport failed")
	}
	text := ""
	for _, part := range result.Content {
		if c, ok := part.(*mcp.TextContent); ok {
			text += c.Text
		}
	}
	if !result.IsError || !strings.Contains(text, "session state is corrupt") || strings.Contains(text, dir) || strings.Contains(text, "synthetic-private") {
		t.Fatal("corrupt startup state lost its safe actionable cause")
	}
	if r.direct.Load() != 0 {
		t.Fatal("corrupt state triggered authorization")
	}
}

func TestMisuseBootstrapPermanentErrorsDoNotRetry(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "synthetic rejection", status) }))
			defer server.Close()
			u := newTestBrokerUpstream(t.Context(), server.URL+"/mcp?project=brk")
			defer u.Close()
			retries := 0
			if _, err := initialBrokerTools(t.Context(), u, func(int) time.Duration { retries++; return 0 }); err == nil || retries != 0 {
				t.Fatal("permanent startup failure retried")
			}
		})
	}
}
func TestMisuseBootstrapOutageBackoffCancels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "synthetic outage", 503) }))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	u := newTestBrokerUpstream(ctx, server.URL+"/mcp?project=brk")
	defer u.Close()
	var waits []time.Duration
	defaults := (durableOptions{backoff: resumeFixtureBackoff}).withDefaults()
	_, err := initialBrokerTools(ctx, u, func(attempt int) time.Duration {
		waits = append(waits, defaults.backoff(attempt))
		cancel()
		return time.Hour
	})
	if !errors.Is(err, context.Canceled) || len(waits) != 1 || waits[0] != time.Second {
		t.Fatal("startup outage ignored bounded schedule or cancellation")
	}
}
func TestMisuseB2LocalStateErrorsNeverEchoUnknownInput(t *testing.T) {
	opts := durableOptions{}
	for _, text := range []string{"session state is unreadable", "client id file is invalid; remove it to generate a new identity", "unexpected synthetic-private/path"} {
		err := opts.localRegistrationFailure(broker.AttachRequest{}, errors.New(text))
		var safe *localStateError
		if errors.As(err, &safe) != !strings.Contains(text, "synthetic-private") {
			t.Fatal("local diagnostic allowlist did not fail closed")
		}
	}
}

func TestMisuseB2RunningStateDeletionAndCorruptionRecoverFromMemory(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "deleted", true: "truncated"}[corrupt], func(t *testing.T) {
			u := newSwitchableUpstream(t)
			dir := t.TempDir()
			f := newDurableFixture(t, u, dir)
			first, result := f.attach(t, "misuse-state-owner")
			if result.IsError {
				t.Fatal("initial attach failed")
			}
			key := clientstate.RegistrationKey("https://broker/mcp?project=brk", f.identity.Harness, f.identity.SessionID)
			// Obtain the real scope/key from the local registration files' structure,
			// never read or display their credential-bearing contents.
			files, err := os.ReadDir(filepath.Join(dir, "registrations"))
			if err != nil {
				t.Fatal("registration listing failed")
			}
			for _, file := range files {
				if strings.HasSuffix(file.Name(), ".json") {
					key = strings.TrimSuffix(file.Name(), ".json")
				}
			}
			file := filepath.Join(dir, "registrations", key+".json")
			if corrupt {
				err = os.WriteFile(file, []byte("{truncated"), 0600)
			} else {
				err = os.Remove(file)
			}
			if err != nil {
				t.Fatal("runtime state disruption failed")
			}
			u.restart()
			waitFor(t, "memory-backed resume", func() bool {
				session, err := u.session(first.Name)
				return err == nil && session.AutomaticDeliveryReady && session.Revision > first.Revision
			})
			store, err := clientstate.Open(dir)
			if err != nil {
				t.Fatal("state reopen failed")
			}
			waitFor(t, "state regenerated", func() bool {
				saved, found, err := store.LoadRegistration(key)
				return err == nil && found && saved.RegistrationID == first.RegistrationID && saved.Revision > first.Revision
			})
		})
	}
}

// A bare transport error cannot distinguish a deleted project from an outage.
// Keep this bounded, cancelable behavior explicit until the PM chooses a
// server reason/protocol; a client must not infer ownership loss from 404 text.
func TestMisuseB4UnclassifiedProjectFailureHasBoundedBackoff(t *testing.T) {
	b, opts := suspendClockBinding(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var delays []time.Duration
	opts.resumeClock = devicePollClock{sleep: func(ctx context.Context, d time.Duration) error {
		delays = append(delays, d)
		if len(delays) == 6 {
			cancel()
		}
		return ctx.Err()
	}}
	u := &clockedResumeUpstream{brokerUpstream: &brokerUpstream{}, respond: func() (broker.RegisterResult, error) {
		return broker.RegisterResult{}, errors.New("synthetic project unavailable")
	}}
	if _, ok := resumeDurable(ctx, u, b, opts, ""); ok {
		t.Fatal("failed project looked registered")
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	if !slices.Equal(delays, want) {
		t.Fatal("project failure retry schedule changed or became tight")
	}
}

func TestMisuseB3SavedScopedCredentialSurvivesStartupOutage(t *testing.T) {
	dir := t.TempDir()
	up := newSwitchableUpstream(t)
	first := recoveryFixture(t, dir, up, func(w http.ResponseWriter, r *http.Request) { recoveryGrant(w, 1) })
	waitFor(t, "first registration saved", func() bool { return first.lastSavedRegistrationID() != "" })
	original := first.lastSavedRegistrationID()
	first.stop()
	auth := newDeviceAuthServer(t)
	handler := auth.server.Config.Handler
	var outage atomic.Bool
	outage.Store(true)
	var checks atomic.Int32
	auth.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/capabilities" {
			checks.Add(1)
			if outage.Load() {
				http.Error(w, "synthetic outage", 503)
				return
			}
			writeJSON(w, 200, map[string]any{"version": 1, "capabilities": []string{"scoped-session-v1", "direct-registration-v1"}})
			return
		}
		handler.ServeHTTP(w, r)
	})
	resumed := newDeviceModeFixture(t, auth, up, dir, singleIdentity(), func(f *deviceModeFixture) { f.deferAttach = true })
	// Wait for the initial three bounded discovery attempts to finish. Restore
	// service without any tool call, then require the background startup task.
	waitFor(t, "initial scoped discovery outage", func() bool { return checks.Load() >= 3 })
	time.Sleep(50 * time.Millisecond)
	outage.Store(false)
	waitFor(t, "saved scoped registration resumes without tool call", func() bool {
		resumed.mu.Lock()
		defer resumed.mu.Unlock()
		for _, state := range resumed.saved {
			if state.Registration.RegistrationID == original && state.Registration.Revision > 1 {
				return true
			}
		}
		return false
	})
	if resumed.codeRequests() != 0 || resumed.log.count("register_session") != 0 {
		t.Fatal("outage triggered fresh admission instead of saved resume")
	}
}

type misuseTransportFunc func(*http.Request) (*http.Response, error)

func (f misuseTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestMisuseBootstrapConnectionRefusedRecovers(t *testing.T) {
	central := recoveryBroker(t)
	u := newTestBrokerUpstream(t.Context(), central.URL+"/mcp?project=brk")
	defer u.Close()
	var offline atomic.Bool
	offline.Store(true)
	u.transport = misuseTransportFunc(func(r *http.Request) (*http.Response, error) {
		if offline.Load() {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	retries := 0
	tools, err := initialBrokerTools(t.Context(), u, func(int) time.Duration { retries++; offline.Store(false); return 0 })
	if err != nil || len(tools) == 0 || retries != 1 {
		t.Fatal("connection-refused startup did not recover")
	}
}
func TestMisuseB5UnavailableParentNeverRequestsAdmission(t *testing.T) {
	auth := newDeviceAuthServer(t)
	f := newDeviceModeFixture(t, auth, newSwitchableUpstream(t), t.TempDir(), map[string]broker.AttachRequest{}, func(f *deviceModeFixture) { f.deferAttach = true })
	result := f.call("", "auth_status", map[string]any{})
	if !result.IsError || f.codeRequests() != 0 || f.tokenRequests() != 0 || f.log.count("register_session") != 0 {
		t.Fatal("unavailable parent created authorization or registration")
	}
}
func TestMisuseB8AnotherProjectTokenIsRejected(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Query().Get("project") != "other" {
			t.Error("wrong configured project sent")
		}
		writeJSON(w, 401, map[string]string{"error": "invalid_client"})
	}))
	defer server.Close()
	client := newDeviceAuthClient(server.URL+"/mcp?project=other", fixtureToken("asb_p_"), server.Client())
	_, err := client.requestCode(t.Context(), deviceCodeRequest{ClientID: "synthetic-client", Harness: "claude", SessionID: claudeHandoverIdentity(1560, 21560).SessionID, RequestID: "synthetic-request"})
	if !errors.Is(err, errProjectTokenRejected) || requests.Load() != 1 {
		t.Fatal("other-project credential did not stop at rejection")
	}
}
func TestMisuseB9RestartKeepsRegistrationButVolatileQueueIsLost(t *testing.T) {
	u := newSwitchableUpstream(t)
	id := claudeHandoverIdentity(1570, 21570)
	request := broker.RegisterRequest{Harness: "claude", SessionID: id.SessionID, ClientID: "synthetic-client", PID: id.PID, CWD: id.CWD, Name: "restart-owner", Secret: clientstate.NewSecret(), RequestID: "synthetic-first", ChannelID: "synthetic-lease"}
	var registered broker.RegisterResult
	if err := call(t.Context(), u, "register_session", request, &registered); err != nil || registered.Code != "" {
		t.Fatal("fixture registration failed")
	}
	if _, err := u.service.SendMessage(t.Context(), broker.Message{From: registered.Session.Name, To: registered.Session.Name, Body: "volatile-envelope"}); err != nil {
		t.Fatal("queue setup failed")
	}
	before, err := u.session(registered.Session.Name)
	if err != nil || before.PendingMessages != 1 {
		t.Fatal("queue was not populated")
	}
	u.restart()
	after, err := u.session(registered.Session.Name)
	if err != nil || after.RegistrationID != registered.Session.RegistrationID || after.PendingMessages != 0 {
		t.Fatal("persistent-registration versus volatile-queue contract changed")
	}
}
