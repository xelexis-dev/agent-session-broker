package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
)

type scopeHTTPFixture struct {
	raw                   *brokerUpstream
	remote                *broker.RemoteService
	mu                    sync.Mutex
	scopes                []string
	generation            string
	calls                 map[string]int
	legacy, revoked       atomic.Bool
	holdWait, releaseWait chan struct{}
	holdOnce              sync.Once
}

func newScopeHTTPFixture(t *testing.T) *scopeHTTPFixture {
	f := &scopeHTTPFixture{remote: broker.NewRemoteService(), scopes: []string{access.SessionRegister}, calls: map[string]int{}}
	f.remote.SetAdmission(scopeAdmission{})
	handler := mcpserver.Handler(mcpserver.New(f.remote, noMemory{}))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.revoked.Load() {
			w.WriteHeader(401)
			return
		}
		f.mu.Lock()
		scopes := slices.Clone(f.scopes)
		generation := f.generation
		f.mu.Unlock()
		if r.Method == "POST" {
			data, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(data))
			var rpc struct{ Params struct{ Name string } }
			_ = json.Unmarshal(data, &rpc)
			if rpc.Params.Name != "" {
				f.mu.Lock()
				f.calls[rpc.Params.Name]++
				f.mu.Unlock()
			}
			if rpc.Params.Name == "wait_for_message" && f.holdWait != nil {
				f.holdOnce.Do(func() {
					close(f.holdWait)
					select {
					case <-f.releaseWait:
					case <-r.Context().Done():
					}
				})
			}
		}
		ctx := access.WithGrant(r.Context(), access.Grant{Scopes: scopes, ProjectGenerationID: generation})
		if !f.legacy.Load() {
			handler.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r.WithContext(ctx))
		for k, v := range response.Header() {
			w.Header()[k] = v
		}
		data := response.Body.Bytes()
		var payload any
		if json.Unmarshal(data, &payload) == nil {
			stripScopeSignals(payload)
			data, _ = json.Marshal(payload)
			w.Header().Del("Content-Length")
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(data)
	}))
	f.raw = newTestBrokerUpstream(t.Context(), server.URL+"/mcp")
	f.raw.setBearerToken("synthetic-scope-credential")
	t.Cleanup(func() { _ = f.remote.Close(); _ = f.raw.Close(); server.Close() })
	return f
}
func (f *scopeHTTPFixture) set(scopes []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scopes = slices.Clone(scopes)
}
func (f *scopeHTTPFixture) count(tool string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[tool]
}

func TestAuthenticatedRenewRestoresReceiveWithoutToolCall(t *testing.T) {
	f := newScopeHTTPFixture(t)
	state, _ := clientstate.Open(t.TempDir())
	opts := (durableOptions{state: state, scope: "wire", heartbeat: 10 * time.Millisecond, timeout: time.Second, backoff: func(int) time.Duration { return time.Millisecond }, receiverIdentity: fixtureReceiverIdentity{}}).withDefaults()
	id := broker.AttachRequest{Name: "restricted", Harness: "codex", SessionID: "11111111-1111-4111-8111-111111115503", PID: 5503, CWD: "/synthetic", ChannelID: "wire-lease"}
	b := &ingressBinding{mu: &sync.Mutex{}, identity: id, durable: &durableRegistration{key: clientstate.RegistrationKey("wire", "codex", id.SessionID), clientID: "wire-client", secret: clientstate.NewSecret()}}
	u := opts.wrapReceive(f.raw, "synthetic-scope-credential")
	session, err := registerDurable(t.Context(), u, b, opts, id)
	if err != nil {
		t.Fatal("fixture register", err)
	}
	if !receiveDenied(u) {
		t.Fatal("authenticated startup did not apply receive absence")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	delivered := make(chan broker.Message, 2)
	go func() {
		defer close(done)
		runDurableReceiver(ctx, u, b, opts, func(_ context.Context, m broker.Message) error { delivered <- m; return nil })
	}()
	defer func() { cancel(); <-done }()
	waitFor(t, "forbidden heartbeat without open receive", func() bool { return f.count("renew_lease") >= 3 })
	if f.count("wait_for_message") != 0 {
		t.Fatal("startup receive absence opened a receive request")
	}
	initialRows, _ := f.remote.ListSessions(t.Context())
	if len(initialRows) != 1 || initialRows[0].AutomaticDeliveryReady {
		t.Fatal("startup scope denial kept the lease incorrectly ready")
	}
	beforeRestoreResumes := f.count("resume_session")

	f.set([]string{access.MessageReceive, access.SessionRegister})
	waitFor(t, "renew restored exactly one receive worker", func() bool { return f.count("wait_for_message") == 1 })
	rows, _ := f.remote.ListSessions(t.Context())
	if len(rows) != 1 || rows[0].RegistrationID != session.RegistrationID || !rows[0].AutomaticDeliveryReady || f.count("resume_session") != beforeRestoreResumes+1 || f.count("register_session") != 1 {
		t.Fatal("scope restoration changed registration or readiness, or duplicated resume")
	}
	if _, err := f.remote.SendMessage(t.Context(), broker.Message{From: session.Name, To: session.Name, Body: "heartbeat-only scope restoration"}); err != nil {
		t.Fatal("fixture send", err)
	}
	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Fatal("restored native sink did not deliver")
	}
	current := authScopes(t.Context(), u, authStatus{Status: authAuthorized})
	if !slices.Equal(current.Scopes, []string{access.MessageReceive, access.SessionRegister}) || current.ScopeSource != "server" || f.count("session_token_info") != 0 {
		t.Fatal("auth_status did not use the same accepted scope observation")
	}
	unchangedAt := f.count("renew_lease")
	unchangedResumes := f.count("resume_session")
	waitFor(t, "unchanged authenticated snapshots", func() bool { return f.count("renew_lease") >= unchangedAt+4 })
	if f.count("resume_session") != unchangedResumes {
		t.Fatal("identical authenticated scopes restarted reception")
	}
	// A revoked credential cannot be revived by its previously accepted scope list.
	f.revoked.Store(true)
	waitFor(t, "credential revocation stops receiver", func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
	if f.count("register_session") != 1 {
		t.Fatal("revocation created a registration")
	}
}

func TestOldIncarnationDenialAndCapabilityDisappearance(t *testing.T) {
	f := newScopeHTTPFixture(t)
	f.holdWait = make(chan struct{})
	f.releaseWait = make(chan struct{})
	initial, err := f.raw.session(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer initial.Close()
	oldResult := make(chan error, 1)
	go func() {
		_, err := f.raw.CallTool(t.Context(), &mcp.CallToolParams{Name: "wait_for_message", Arguments: map[string]string{"recipient": "unused"}})
		oldResult <- err
	}()
	select {
	case <-f.holdWait:
	case <-time.After(time.Second):
		t.Fatal("old request was not held")
	}
	f.set([]string{access.MessageReceive, access.SessionRegister})
	f.raw.mu.Lock()
	f.raw.current = nil
	f.raw.mu.Unlock()
	if _, err := f.raw.session(t.Context()); err != nil {
		t.Fatal(err)
	}
	close(f.releaseWait)
	if err := <-oldResult; !errors.Is(err, errStaleScopeObservation) {
		t.Fatal("old-incarnation insufficient_scope was not fenced")
	}
	opts := (durableOptions{}).withDefaults()
	u := opts.wrapReceive(f.raw, "synthetic-scope-credential")
	if receiveDenied(u) {
		t.Fatal("old denial poisoned new grant")
	}
	u.(*receiveUpstream).currentPermission().decide(receiveForbidden)
	f.legacy.Store(true)
	f.raw.mu.Lock()
	previous := f.raw.current
	f.raw.current = nil
	f.raw.mu.Unlock()
	defer previous.Close()
	if _, err := f.raw.session(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !receiveDenied(u) {
		t.Fatal("capability disappearance guessed full access instead of preserving sticky denial")
	}
}

func TestScopeInitializeFromReplacedProjectGenerationIsUnknown(t *testing.T) {
	f := newScopeHTTPFixture(t)
	f.mu.Lock()
	f.generation = strings.Repeat("B", 26)
	f.scopes = []string{access.MessageReceive, access.SessionRegister}
	f.mu.Unlock()
	f.raw.projectState, _ = newProjectTracker(nil, "scope")
	f.raw.expectedProjectGeneration = strings.Repeat("A", 26)
	if _, err := f.raw.session(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, scopes := f.raw.receiveScopeObservation(); scopes != nil {
		t.Fatal("replacement project's initialize taught receive permission for old registration")
	}
	if _, err := f.raw.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_sessions", Arguments: struct{}{}}); !terminalProjectError(err) {
		t.Fatal("project terminal guard did not win")
	}
}
