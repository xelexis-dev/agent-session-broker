package channel

import (
	"context"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestProjectRefusalTrustProvenanceAndReset(t *testing.T) {
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := newProjectTracker(state, "scope-a")
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Repeat("A", 26)
	source := credentialSource("synthetic-credential")
	parent := credentialSource("synthetic-admission")
	if terminalProjectError(p.refusal(source, 410, []byte(`{"error":"project_deleted"}`))) {
		t.Fatal("untrusted refusal taught support")
	}
	p.initialized(source, parent, &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{Experimental: map[string]any{protocol.ProjectRefusalCapability: map[string]any{"projectGenerationId": old}}}})
	for _, tc := range []struct {
		status int
		body   string
	}{{410, ""}, {410, "<html>proxy</html>"}, {404, `{"error":"unknown"}`}, {403, `{"error":"project_deleted"}`}} {
		if p.refusal(source, tc.status, []byte(tc.body)) != nil {
			t.Fatal("proxy or unknown response became terminal")
		}
	}
	if p.hint(parent) != old {
		t.Fatal("admission provenance not associated")
	}
	if p.hint(credentialSource("replacement-admission")) != "" {
		t.Fatal("superseded source hint leaked")
	}
	if !terminalProjectError(p.refusal(source, 410, []byte(`{"error":"project_deleted"}`))) {
		t.Fatal("known deleted generation did not stop")
	}
	reloaded, err := newProjectTracker(state, "scope-a")
	if err != nil || !terminalProjectError(reloaded.terminal(source)) {
		t.Fatal("terminal did not survive restart")
	}
	p.reset(source)
	if p.hint(source) != "" || p.terminal(source) != nil {
		t.Fatal("explicit recovery retained old hint or terminal")
	}
	p.initialized(source, parent, &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{}})
	if p.hint(source) != "" || terminalProjectError(p.refusal(source, 410, []byte(`{"error":"project_deleted"}`))) {
		t.Fatal("authenticated unsupported negotiation retained knowledge")
	}
	other, _ := newProjectTracker(state, "scope-b")
	if other.hint(source) != "" {
		t.Fatal("scope leak")
	}
}

func TestProjectRefusalDirectDeviceAndPollStopWithoutRetry(t *testing.T) {
	for _, operation := range []string{"direct", "device", "poll"} {
		t.Run(operation, func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get(protocol.ProjectGenerationHeader) != strings.Repeat("A", 26) {
					t.Error("missing learned hint")
				}
				calls.Add(1)
				w.WriteHeader(410)
				w.Write([]byte(`{"error":"project_deleted"}`))
			}))
			defer s.Close()
			c := newInstantDeviceAuthClient(s.URL+"/mcp?project=p", "synthetic-admission", s.Client())
			c.projectState, _ = newProjectTracker(nil, "scope")
			c.projectState.learn(c.projectSource, "", strings.Repeat("A", 26), true)
			invoke := func() error {
				switch operation {
				case "direct":
					_, err := c.exchange(t.Context(), directCodeRequest{Recovery: true})
					return err
				case "device":
					_, err := c.requestCode(t.Context(), deviceCodeRequest{})
					return err
				default:
					_, _, err := pollDeviceToken(t.Context(), c, "synthetic-code", time.Second, time.Now().Add(time.Hour), realPollClock())
					return err
				}
			}
			if err := invoke(); !terminalProjectError(err) {
				t.Fatalf("lost project terminal: %v", err)
			}
			if err := invoke(); !terminalProjectError(err) || calls.Load() != 1 {
				t.Fatal("terminal retried")
			}
		})
	}
}

func TestProjectUnavailableRemainsRetryableAndCancellationWorks(t *testing.T) {
	p, _ := newProjectTracker(nil, "scope")
	p.learn("source", "", strings.Repeat("A", 26), true)
	for _, code := range []string{"project_disabled", "project_deleting"} {
		err := p.refusal("source", 403, []byte(`{"error":"`+code+`"}`))
		var temporary *directTransientError
		if terminalProjectError(err) || !errors.As(err, &temporary) || temporary.retryAfter != 30*time.Second {
			t.Fatal("unavailability became terminal or unbounded retry")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if realPollClock().sleep(ctx, 30*time.Second) != context.Canceled {
		t.Fatal("retry not cancelable")
	}
}

type projectRefusalBackend struct{}

func (projectRefusalBackend) Sessions(context.Context, string) ([]controlpanel.Session, error) {
	return nil, nil
}
func (projectRefusalBackend) Detach(context.Context, string, string) error { return nil }
func (projectRefusalBackend) DeleteProject(context.Context, string) error  { return nil }

func TestProjectDeletedBeforeNextInitializeStopsAndDisabledRecovers(t *testing.T) {
	store, err := controlpanel.OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.Setup("synthetic-project-password") != nil || store.CreateProject("brk") != nil {
		t.Fatal("setup")
	}
	token, err := store.IssueToken("brk", "synthetic-session")
	if err != nil {
		t.Fatal(err)
	}
	central := recoveryBroker(t)
	server := httptest.NewTLSServer(controlpanel.New(store, projectRefusalBackend{}, central.Config.Handler))
	defer server.Close()
	state, _ := clientstate.Open(t.TempDir())
	tracker, _ := newProjectTracker(state, "scope")
	makeUp := func() *brokerUpstream {
		up := newTestBrokerUpstream(t.Context(), server.URL+"/mcp?project=brk")
		up.projectState = tracker
		up.transport = server.Client().Transport
		up.setBearerToken(token)
		t.Cleanup(func() { up.Close() })
		return up
	}
	first := makeUp()
	if _, err := first.session(t.Context()); err != nil {
		t.Fatal(err)
	}
	generation := tracker.hint(credentialSource(token))
	if generation == "" {
		t.Fatal("authenticated initialize did not teach generation")
	}
	var info access.TokenInfo
	if err := call(t.Context(), first, access.TokenInfoTool, struct{}{}, &info); err != nil || info.ProjectGenerationID != generation || !info.ProjectRefusals {
		t.Fatal("authenticated token-info lost generation")
	}
	first.Close()
	if store.SaveProject("brk", "token", false, "") != nil {
		t.Fatal("disable")
	}
	second := makeUp()
	var attempts atomic.Int32
	_, err = initialBrokerTools(t.Context(), second, func(int) time.Duration {
		attempts.Add(1)
		if store.SaveProject("brk", "token", true, "") != nil {
			t.Error("reenable")
		}
		return time.Millisecond
	})
	if err != nil || attempts.Load() != 1 {
		t.Fatalf("disabled did not recover: %v", err)
	}
	second.Close()
	// Delete and recreate through the real panel API; no token tombstone survives.
	jar, _ := cookiejar.New(nil)
	browser := *server.Client()
	browser.Jar = jar
	browser.CheckRedirect = noRedirect
	csrfPattern := regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	csrf := ""
	request := func(path string, form url.Values) {
		t.Helper()
		var response *http.Response
		var err error
		if form == nil {
			response, err = browser.Get(server.URL + path)
		} else {
			form.Set("csrf", csrf)
			response, err = browser.PostForm(server.URL+path, form)
		}
		if err != nil {
			t.Fatal("panel request failed")
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != 200 && response.StatusCode != 303 {
			t.Fatalf("panel request status %d", response.StatusCode)
		}
		if match := csrfPattern.FindSubmatch(body); len(match) == 2 {
			csrf = string(match[1])
		}
	}
	request("/login", nil)
	request("/login", url.Values{"username": {"admin"}, "password": {"synthetic-project-password"}})
	request("/", nil)
	request("/projects/delete", url.Values{"project": {"brk"}, "confirmName": {"brk"}})
	if store.CreateProject("brk") != nil {
		t.Fatal("recreate")
	}
	third := makeUp()
	_, err = initialBrokerTools(t.Context(), third, func(int) time.Duration { t.Fatal("deleted project kept retrying"); return 0 })
	if !terminalProjectError(err) {
		t.Fatalf("deleted project did not stop: %v", err)
	}
	if tracker.generation(credentialSource(token)) != generation {
		t.Fatal("refusal overwrote trusted generation")
	}
}

func TestProjectTerminalResumePreservesSavedRegistration(t *testing.T) {
	tracker, _ := newProjectTracker(nil, "scope")
	source := credentialSource("synthetic-credential")
	tracker.learn(source, "", strings.Repeat("A", 26), true)
	tracker.refusal(source, 404, []byte(`{"error":"project_not_found"}`))
	up := newTestBrokerUpstream(t.Context(), "https://unused.example/mcp?project=p")
	defer up.Close()
	up.setBearerToken("synthetic-credential")
	up.projectState = tracker
	binding := &ingressBinding{mu: &sync.Mutex{}, identity: protocol.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: "/synthetic"}, durable: &durableRegistration{registrationID: strings.Repeat("A", 26), secret: "synthetic-proof", revision: 7, clientID: "synthetic-client"}}
	opts := (durableOptions{backoff: func(int) time.Duration { t.Fatal("terminal resumed again"); return 0 }}).withDefaults()
	if _, ok := resumeDurable(t.Context(), up, binding, opts, ""); ok {
		t.Fatal("deleted resume succeeded")
	}
	if binding.durable.registrationID != strings.Repeat("A", 26) || binding.durable.secret != "synthetic-proof" || binding.durable.revision != 7 {
		t.Fatal("deleted project discarded saved proof")
	}
	if binding.resumeStopReason != "project-deleted" || !strings.Contains(binding.receiveReason, "preserved") {
		t.Fatal("terminal diagnostic missing")
	}
}

func TestProjectRecoveryFirstRequestDoesNotReuseSupersededHint(t *testing.T) {
	var hint atomic.Value
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hint.Store(r.Header.Get(protocol.ProjectGenerationHeader))
		writeDirectGrant(w, []string{"session:register"})
	}))
	defer server.Close()
	c := newInstantDeviceAuthClient(server.URL+"/mcp?project=p", "synthetic-new-admission", server.Client())
	c.projectState, _ = newProjectTracker(nil, "scope")
	oldCredential := "synthetic-old-credential"
	oldSource := credentialSource(oldCredential)
	c.projectState.learn(oldSource, "", strings.Repeat("A", 26), true)
	c.projectState.refusal(oldSource, 410, []byte(`{"error":"project_deleted"}`))
	auth := &sessionAuth{state: clientstate.SessionState{AdmissionFingerprint: credentialSource("synthetic-old-admission"), Credential: &clientstate.SessionCredential{AccessToken: oldCredential}, Registration: clientstate.Registration{RegistrationID: strings.Repeat("B", 26), Secret: "synthetic-proof", Revision: 9}}}
	opts := durableOptions{device: &deviceAuthOptions{authorizer: c}}
	opts.resetSupersededProjectSource(auth)
	if auth.state.Credential != nil || auth.state.Registration.Secret != "synthetic-proof" || auth.state.Registration.Revision != 9 || auth.state.Registration.RegistrationID != strings.Repeat("B", 26) {
		t.Fatal("token change did not reset terminal while preserving registration")
	}
	if c.projectState.hint(oldSource) != "" {
		t.Fatal("superseded credential hint still active")
	}
	if _, err := c.exchange(t.Context(), directCodeRequest{}); err != nil {
		t.Fatal(err)
	}
	if hint.Load() != "" {
		t.Fatal("first recovery request reused superseded hint")
	}
}

func TestProjectExplicitRecoveryCannotCreateRegistrationInNewGeneration(t *testing.T) {
	central := recoveryBroker(t)
	tracker, _ := newProjectTracker(nil, "scope")
	up := newTestBrokerUpstream(t.Context(), central.URL+"/mcp?project=brk")
	defer up.Close()
	up.projectState = tracker
	oldID := strings.Repeat("A", 26)
	tracker.entries["registration:"+oldID] = clientstate.ProjectKnowledge{GenerationID: strings.Repeat("B", 26), Terminal: "project_deleted"}
	state, _ := clientstate.Open(t.TempDir())
	id := protocol.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: "/synthetic", ChannelID: "lease", Name: "worker"}
	binding := &ingressBinding{mu: &sync.Mutex{}, identity: id, durable: &durableRegistration{registrationID: oldID, secret: strings.Repeat("s", 40), revision: 7, clientID: "synthetic-client", key: "synthetic-key"}}
	opts := durableOptions{state: state, backoff: func(int) time.Duration { t.Fatal("not-found retried"); return 0 }}
	if _, err := registerProjectSafe(t.Context(), up, binding, opts, id); !terminalProjectError(err) {
		t.Fatalf("recovery did not refuse: %v", err)
	}
	if binding.durable.registrationID != oldID || binding.durable.revision != 7 {
		t.Fatal("explicit recovery replaced saved registration")
	}
	var sessions []protocol.Session
	if err := call(t.Context(), up, "list_sessions", struct{}{}, &sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatal("explicit recovery created a fresh registration")
	}
}

func TestProjectLateAuthenticatedResponseCannotRestartStoppedSource(t *testing.T) {
	p, _ := newProjectTracker(nil, "scope")
	p.learn("credential", "admission", strings.Repeat("A", 26), true)
	p.refusal("credential", 410, []byte(`{"error":"project_deleted"}`))
	// This reply was authenticated before deletion, but completed after the refusal.
	p.learn("credential", "admission", strings.Repeat("A", 26), true)
	if !terminalProjectError(p.terminal("credential")) {
		t.Fatal("late authenticated response erased project terminal")
	}
}

func TestProjectChildProvenanceAndExplicitRecoveryFenceLateLearning(t *testing.T) {
	p, _ := newProjectTracker(nil, "scope")
	p.learn("old-child", "admission", strings.Repeat("A", 26), true)
	sourceRevision, parentRevision := p.versions("old-child", "admission")
	p.reset("old-child")
	p.reset("admission")
	p.learn("old-child", "admission", strings.Repeat("A", 26), true, sourceRevision, parentRevision)
	if p.hint("old-child") != "" || p.hint("admission") != "" {
		t.Fatal("late learning reactivated superseded hint")
	}
	p.inherit("replacement-child", "admission")
	if p.hint("replacement-child") != "" {
		t.Fatal("new child inherited superseded hint")
	}
	p.learn("current-child", "admission", strings.Repeat("B", 26), true)
	p.inherit("next-child", "admission")
	if p.hint("next-child") != strings.Repeat("B", 26) {
		t.Fatal("current provenance lost")
	}
	p.refusal("current-child", 410, []byte(`{"error":"project_deleted"}`))
	p.inherit("late-issued-child", "admission")
	if !terminalProjectError(p.terminal("late-issued-child")) {
		t.Fatal("late issued child bypassed project terminal")
	}
}
