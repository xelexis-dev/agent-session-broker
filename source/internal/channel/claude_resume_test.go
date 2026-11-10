package channel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// The session file changes after MCP initialization, without another model
// tool call. The client must abandon the startup code and resume the saved
// conversation; pinning the first ID strands it in pending authorization.
func TestClaudeResumeReobservesPendingIdentity(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "channel", true: "native"}[native], func(t *testing.T) { testClaudeResumeReobservesPendingIdentity(t, native) })
	}
}
func testClaudeResumeReobservesPendingIdentity(t *testing.T, native bool) {
	if native {
		t.Setenv("CLAUDE_CODE_SESSION_ID", "22222222-2222-4222-8222-222222222222")
		t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "synthetic-only")
		t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "/private/tmp/synthetic-inbox.sock")
	}
	const resumedID = "11111111-1111-4111-8111-111111111111"
	const transientID = "22222222-2222-4222-8222-222222222222"
	auth, upstream := newDeviceAuthServer(t), newSwitchableUpstream(t)
	stateDir := t.TempDir()
	first := newDeviceModeFixture(t, auth, upstream, stateDir, singleIdentity())
	first.grant(resumedID, "fixture-credential", "agent-one")
	waitFor(t, "saved original registration", func() bool { return first.lastSavedRegistrationID() != "" })
	original, err := upstream.session("agent-one")
	if err != nil {
		t.Fatal("original registration unavailable")
	}
	first.stop()

	state, err := clientstate.Open(stateDir)
	if err != nil {
		t.Fatal("state fixture unavailable")
	}
	sessionFile := filepath.Join(t.TempDir(), "4242.json")
	initial := singleIdentity()[resumedID]
	initial.SessionID = transientID
	writeIdentity := func(id broker.AttachRequest) {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"sessionId": id.SessionID, "pid": id.PID, "cwd": id.CWD, "kind": "interactive"})
		// Atomic replacement mirrors the harness update; the observer reads only
		// these selected identity fields, never transcript content or task paths.
		next := sessionFile + ".next"
		if os.WriteFile(next, data, 0600) != nil || os.Rename(next, sessionFile) != nil {
			t.Fatal("session fixture write failed")
		}
	}
	writeIdentity(initial)
	observe := func(ctx context.Context, _ broker.AttachRequest) (broker.AttachRequest, error) {
		data, err := os.ReadFile(sessionFile)
		if err != nil {
			return broker.AttachRequest{}, err
		}
		var row struct {
			SessionID string `json:"sessionId"`
			PID       int    `json:"pid"`
			CWD       string `json:"cwd"`
			Kind      string `json:"kind"`
		}
		if err = json.Unmarshal(data, &row); err != nil {
			return broker.AttachRequest{}, err
		}
		return nearestClaudeIdentity(ctx, 4242, func(context.Context) ([]rosterRow, error) {
			return []rosterRow{{SessionID: row.SessionID, PID: row.PID, Kind: row.Kind}}, nil
		}, func(_ context.Context, id string) (broker.AttachRequest, error) {
			return broker.AttachRequest{Harness: "claude", SessionID: id, PID: row.PID, CWD: row.CWD}, nil
		}, time.Second, time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pollStarted, pollCanceled := make(chan struct{}), make(chan struct{})
	var startedOnce, canceledOnce sync.Once
	opts := durableOptions{claudeStartupClock: instantClaudeClock(), enabled: true, tokenMode: true, state: state, project: "brk", scope: "https://broker/mcp?project=brk", observe: observe, heartbeat: 50 * time.Millisecond, timeout: time.Second, backoff: func(int) time.Duration { return 10 * time.Millisecond }, device: &deviceAuthOptions{
		authorizer: newDeviceAuthClient(auth.server.URL+"/mcp?project=brk", "fixture-project", auth.server.Client()),
		newUpstream: func(credential string) Upstream {
			return &recordingUpstream{bearer: credential, inner: upstream, log: &callLog{}}
		},
		clock: devicePollClock{now: time.Now, sleep: func(ctx context.Context, _ time.Duration) error {
			startedOnce.Do(func() { close(pollStarted) })
			<-ctx.Done()
			canceledOnce.Do(func() { close(pollCanceled) })
			return ctx.Err()
		}},
	}}
	st, ct := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	if native {
		opts.claudeInboxFactory = newClaudeInboxFactory(ctx, initial, observe, func(context.Context, int) (string, error) { return "same-process-start", nil })
		identify := claudeConversationIdentity(initial, &opts)
		go func() { done <- ServeDurableIngress(ctx, st, unavailableUpstream{}, snapshotTools(), identify, opts) }()
	} else {
		go func() { done <- ServeDurableChannel(ctx, st, unavailableUpstream{}, snapshotTools(), initial, opts) }()
	}
	local, err := mcp.NewClient(&mcp.Implementation{Name: "fixture", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		cancel()
		t.Fatal("fixture MCP connect failed")
	}
	defer local.Close()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("client shutdown did not join its workers")
		}
	}()
	_, _ = local.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
	select {
	case <-pollStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("startup device poll did not begin")
	}
	current := initial
	current.SessionID = resumedID
	writeIdentity(current)
	waitFor(t, "stored conversation resumed after identity changed", func() bool {
		s, e := upstream.session("agent-one")
		return e == nil && s.RegistrationID == original.RegistrationID && s.ID == resumedID && s.Revision > original.Revision
	})
	select {
	case <-pollCanceled:
	case <-time.After(time.Second):
		t.Fatal("transient approval poller remained active")
	}
	// Registration pins the conversation for this MCP connection, as before
	// startup re-observation was introduced. In-process /clear or another
	// /resume and temporary unreadable files must not invalidate its tools.
	codeRequests := first.codeRequests()
	for _, change := range []string{"another-conversation", "unreadable-record"} {
		if change == "another-conversation" {
			writeIdentity(initial)
		} else if os.WriteFile(sessionFile, []byte("{"), 0600) != nil {
			t.Fatal("failed to corrupt selected-fields fixture")
		}
		result, err := local.CallTool(ctx, &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
		if err != nil || result.IsError {
			t.Fatalf("registered conversation tool failed after %s", change)
		}
		if first.codeRequests() != codeRequests {
			t.Fatal("registered conversation started a new device flow")
		}
		registered, err := upstream.session("agent-one")
		if err != nil || registered.ID != resumedID || registered.RegistrationID != original.RegistrationID {
			t.Fatal("registered conversation identity changed")
		}
	}
	result, err := local.CallTool(ctx, &mcp.CallToolParams{Name: "auth_status", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatal("current auth status unavailable")
	}
	raw, _ := json.Marshal(result.StructuredContent)
	var status struct {
		Status         string `json:"status"`
		Registered     bool   `json:"registered"`
		UserCode       string `json:"userCode"`
		RegistrationID string `json:"registrationId"`
	}
	if json.Unmarshal(raw, &status) != nil || status.Status != "authorized" || !status.Registered || status.UserCode != "" || status.RegistrationID != original.RegistrationID {
		t.Fatal("current identity still shows pending or lost the stored registration")
	}
	clientID, err := state.ClientID()
	if err != nil {
		t.Fatal("fixture client identity unavailable")
	}
	transient, found, err := state.LoadSession(clientstate.SessionKey(opts.scope, "claude", transientID, clientID))
	if err != nil || !found || transient.Device != nil || transient.Credential != nil {
		t.Fatal("transient approval state was not abandoned")
	}
}

// Re-observation must not turn a PID/CWD/harness change or a failed observer
// into a usable identity. Legacy token connections retain their fixed ID.
func TestClaudeConversationIdentityKeepsParentBoundary(t *testing.T) {
	initial := singleIdentity()["11111111-1111-4111-8111-111111111111"]
	for _, test := range []struct {
		name   string
		change func(*broker.AttachRequest)
		fail   bool
	}{
		{"resumed", func(id *broker.AttachRequest) { id.SessionID = "22222222-2222-4222-8222-222222222222" }, false},
		{"pid", func(id *broker.AttachRequest) { id.PID++ }, true},
		{"cwd", func(id *broker.AttachRequest) { id.CWD = "different" }, true},
		{"harness", func(id *broker.AttachRequest) { id.Harness = "codex" }, true},
		{"empty", func(id *broker.AttachRequest) { id.SessionID = "" }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			observed := initial
			test.change(&observed)
			opts := durableOptions{claudeStartupClock: instantClaudeClock(), device: &deviceAuthOptions{}, observe: func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return observed, nil }}
			id, err := claudeConversationIdentity(initial, &opts)(context.Background(), deviceRequest("ignored-thread-hint"))
			if (err != nil) != test.fail {
				t.Fatal("parent boundary result mismatch")
			}
			if !test.fail && id.SessionID != observed.SessionID {
				t.Fatal("conversation ID was pinned to the initial observation")
			}
			opts.device = nil
			id, err = claudeConversationIdentity(initial, &opts)(context.Background(), nil)
			if err != nil || !reflect.DeepEqual(id, initial) {
				t.Fatal("legacy binding changed")
			}
		})
	}
}
