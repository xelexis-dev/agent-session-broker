package channel

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type fakeResumeControl struct {
	loaded, connected bool
	calls             int
	status            json.RawMessage
}

func (f *fakeResumeControl) Loaded(context.Context) (bool, error)    { return f.loaded, nil }
func (f *fakeResumeControl) Connected(context.Context) (bool, error) { return f.connected, nil }
func (f *fakeResumeControl) AuthStatus(context.Context) (json.RawMessage, error) {
	f.calls++
	return f.status, nil
}
func (f *fakeResumeControl) Close() error { return nil }
func TestDaemonResumeCore(t *testing.T) {
	f := &fakeResumeControl{loaded: true, connected: true, status: json.RawMessage(`{"structuredContent":{"registered":true,"receiverReady":true},"content":[]}`)}
	d := daemonResumeDeps{alive: func() bool { return true }, dial: func(context.Context) (daemonResumeClient, error) { return f, nil }, wait: func(context.Context) error { return context.Canceled }, stopReason: func(error) string { return "" }}
	if reason := runDaemonResume(t.Context(), d); reason != "ready" || f.calls != 1 {
		t.Fatalf("reason=%s auth calls=%d", reason, f.calls)
	}
}

func TestDaemonResumeResultAndAttemptLimits(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
		calls           int
	}{
		{"ready", `{"structuredContent":{"registered":true,"receiverReady":true}}`, "ready", 1},
		{"not-registered", `{"structuredContent":{"registered":false,"receiverReady":false}}`, "not-registered", 1},
		{"old-client", `{"structuredContent":{"registered":true}}`, "unverified", 1},
		{"not-ready", `{"structuredContent":{"registered":true,"receiverReady":false}}`, "attempts-exhausted", 3},
		{"transient", `{"isError":true,"content":[]}`, "attempts-exhausted", 3},
		{"text-result", `{"content":[{"type":"text","text":"{\"registered\":true,\"receiverReady\":true}"}]}`, "ready", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeResumeControl{loaded: true, connected: true, status: json.RawMessage(tc.raw)}
			d := daemonResumeDeps{alive: func() bool { return true }, dial: func(context.Context) (daemonResumeClient, error) { return f, nil }, wait: func(context.Context) error { return nil }}
			if got := runDaemonResume(t.Context(), d); got != tc.want || f.calls != tc.calls {
				t.Fatalf("%s calls=%d", got, f.calls)
			}
		})
	}
}
func TestDaemonResumeWaitsForOnlyLoadedConnectedTarget(t *testing.T) {
	for _, which := range []string{"not-loaded", "not-connected", "dead"} {
		t.Run(which, func(t *testing.T) {
			f := &fakeResumeControl{loaded: which != "not-loaded", connected: which != "not-connected"}
			d := daemonResumeDeps{alive: func() bool { return which != "dead" }, dial: func(context.Context) (daemonResumeClient, error) { return f, nil }, wait: func(context.Context) error { return context.DeadlineExceeded }}
			runDaemonResume(t.Context(), d)
			if f.calls != 0 {
				t.Fatal("premature auth call")
			}
		})
	}
}
func TestResumeTUIPinSurvivesExecAndRejectsReuse(t *testing.T) {
	for _, which := range []string{"exec", "reuse", "never"} {
		t.Run(which, func(t *testing.T) {
			now := time.Unix(1, 0)
			birth, key := "birth-1", "sh"
			codex := false
			s := resumeProcessSource{birth: func(int) (string, error) { return birth, nil }, key: func(int) (string, error) { return key, nil }, codex: func(int) bool { return codex }, now: func() time.Time { return now }, wait: func(context.Context) error {
				now = now.Add(250 * time.Millisecond)
				switch which {
				case "exec":
					codex = true
					key = "codex"
				case "reuse":
					birth = "birth-2"
					codex = true
				case "never":
					now = now.Add(11 * time.Second)
				}
				return nil
			}}
			alive, err := pinResumeTUI(t.Context(), 123, s)
			if which != "exec" {
				if err == nil {
					t.Fatal("unverified target pinned")
				}
				return
			}
			if err != nil || !alive() {
				t.Fatal("legitimate exec lost birth pin")
			}
			key = "changed"
			if alive() {
				t.Fatal("changed Codex key accepted")
			}
		})
	}
}
func TestReceiverReadyRequiresCurrentLiveGeneration(t *testing.T) {
	for _, which := range []string{"ready", "failed", "retired", "unavailable", "reason", "waiting", "stale-generation", "stale-lease", "closed", "not-started", "claim"} {
		t.Run(which, func(t *testing.T) {
			b := &ingressBinding{mu: &sync.Mutex{}, started: true, generation: 4, ingressReady: true, ingressReadyChannel: "L", ingressReadyGeneration: 4, identity: broker.AttachRequest{ChannelID: "L"}, done: make(chan struct{})}
			switch which {
			case "failed":
				b.failed = true
			case "retired":
				b.retired = true
			case "unavailable":
				b.receiveUnavailable = true
			case "reason":
				b.receiveReason = "reconnect-required"
			case "waiting":
				b.holderWaiting = true
			case "stale-generation":
				b.ingressReadyGeneration = 3
			case "stale-lease":
				b.ingressReadyChannel = "old"
			case "closed":
				close(b.done)
			case "not-started":
				b.started = false
			case "claim":
				b.claimThread = "U"
			}
			out := claimAuthStatus(b, authStatus{Registered: true})
			if out.ReceiverReady == nil || *out.ReceiverReady != (which == "ready") {
				t.Fatal("wrong current receiver readiness")
			}
		})
	}
	raw, _ := json.Marshal(authStatus{})
	if strings.Contains(string(raw), "receiverReady") {
		t.Fatal("optional field present when unspecified")
	}
}

func TestDaemonResumeAuthMatchesFirstHookContract(t *testing.T) {
	const thread = "11111111-1111-4111-8111-111111111111"
	type effect struct {
		codes, tokens int
		device        string
		status        string
		registered    bool
	}
	for _, stateKind := range []string{"pending-only", "stale-credential", "fresh"} {
		t.Run(stateKind, func(t *testing.T) {
			run := func(tool string) effect {
				auth := newDeviceAuthServer(t)
				up := newSwitchableUpstream(t)
				dir := t.TempDir()
				store, err := clientstate.Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				cid, err := store.ClientID()
				if err != nil {
					t.Fatal(err)
				}
				if stateKind != "fresh" {
					saved := clientstate.SessionState{Scope: "https://broker/mcp?project=brk", Project: "brk", Harness: "codex", SessionID: thread, ClientID: cid, Registration: clientstate.Registration{Secret: clientstate.NewSecret(), InitialAttempt: initialPending}}
					if stateKind == "stale-credential" {
						saved.Registration.RegistrationID = "old-registration"
						saved.Registration.Name = "old-worker"
						saved.Credential = &clientstate.SessionCredential{AccessToken: "fixture-expired", TokenType: "Bearer", IssuedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour)}
					}
					if err := store.SaveSession(clientstate.SessionKey(saved.Scope, "codex", thread, cid), saved); err != nil {
						t.Fatal(err)
					}
				}
				ids := map[string]broker.AttachRequest{thread: {Harness: "codex", SessionID: thread, PID: 4242, CWD: "/fixture"}, "22222222-2222-4222-8222-222222222222": {Harness: "codex", SessionID: "22222222-2222-4222-8222-222222222222", PID: 4242, CWD: "/fixture"}}
				f := newDeviceModeFixture(t, auth, up, dir, ids, func(f *deviceModeFixture) {
					f.deferAttach = true
					f.sinkFactory = func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
						r := newCodexIngress(func(context.Context, broker.Message) error { return nil })
						return r.deliver, r, nil
					}
				})
				args := map[string]any{}
				if tool == "ingress_hook" {
					args = map[string]any{"session_id": thread, "hook_event_name": "UserPromptSubmit"}
				}
				_ = f.call(thread, tool, args)
				if stateKind == "pending-only" {
					deadline := time.Now().Add(5 * time.Second)
					for {
						f.mu.Lock()
						pendingSaved := false
						for _, saved := range f.saved {
							pendingSaved = pendingSaved || saved.Device != nil && saved.Device.Status == clientstate.DevicePending
						}
						f.mu.Unlock()
						status := f.status(thread)
						if pendingSaved && f.codeRequests() >= 1 && status["status"] == "pending" {
							break
						}
						if time.Now().After(deadline) {
							t.Fatalf("pending contract tool=%s codes=%d tokens=%d observedPending=%v latestDevice=%s authStatus=%v registered=%v", tool, f.codeRequests(), f.tokenRequests(), pendingSaved, f.lastSavedDeviceStatus(), status["status"], status["registered"])
						}
						time.Sleep(10 * time.Millisecond)
					}
				}
				status := f.status(thread)
				finalStatus, _ := status["status"].(string)
				registered, _ := status["registered"].(bool)
				result := effect{f.codeRequests(), f.tokenRequests(), f.lastSavedDeviceStatus(), finalStatus, registered}
				f.stop()
				return result
			}
			helper, hook := run("auth_status"), run("ingress_hook")
			if helper.codes != hook.codes || helper.device != hook.device || helper.status != hook.status || helper.registered != hook.registered {
				t.Fatalf("different first-call effects helper=%+v hook=%+v", helper, hook)
			}
			if stateKind == "pending-only" && helper.codes != 1 {
				t.Fatal("pending attempt was not continued")
			}
			if stateKind == "fresh" && (helper.codes != 0 || helper.tokens != 0) {
				t.Fatal("fresh conversation started authorization")
			}
		})
	}
}
