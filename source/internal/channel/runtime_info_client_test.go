package channel

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/localactivity"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func (u *switchableUpstream) runtimeInfoAllowed() bool { return !u.offline.Load() }
func TestRuntimeInfoRenewNegotiationAndOldServerOmission(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "014-no-version", true: "supported"}[enabled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			pool := newActivityWorkers(ctx)
			defer func() { cancel(); pool.wg.Wait() }()
			up := newSwitchableUpstream(t)
			b := &ingressBinding{mu: &sync.Mutex{}, identity: broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", ChannelID: "lease"}, durable: &durableRegistration{clientID: "client", registrationID: "reg"}, generation: 1, activityEnabled: true, runtimeInfoEnabled: enabled}
			opts := durableOptions{activityPool: pool, heartbeat: time.Second, activityFactory: func(localactivity.Config) activityObserver {
				return observerFunc(func(context.Context, localactivity.Binding) localactivity.Observation {
					return localactivity.Observation{Model: "gpt-6.1-sol", Effort: "medium", State: localactivity.Unknown, Reason: "unsupported", ObservedAt: time.Now().UTC()}
				})
			}}
			stream := opts.newActivityStream(b)
			var activity json.RawMessage
			waitFor(t, "completed metadata observation", func() bool {
				activity = stream.sample(ctx, b, up)
				var observed broker.Activity
				return json.Unmarshal(activity, &observed) == nil && observed.Reason == "unsupported"
			})
			raw, _ := json.Marshal(broker.RenewRequest{Activity: activity, RuntimeInfo: stream.runtimeInfo})
			var args map[string]json.RawMessage
			_ = json.Unmarshal(raw, &args)
			if enabled {
				var info broker.RuntimeInfo
				if json.Unmarshal(args["runtimeInfo"], &info) != nil || info.Model != "gpt-6.1-sol" || info.Effort != "medium" {
					t.Fatal("metadata absent")
				}
			} else if _, ok := args["runtimeInfo"]; ok {
				t.Fatal("old server receives new field")
			}
		})
	}
}
func TestRuntimeInfoInputSchemaGate(t *testing.T) {
	for _, schema := range []map[string]any{{"type": "object", "properties": map[string]any{}}, {"type": "object", "properties": map[string]any{"runtimeInfo": map[string]any{"type": "string"}}}, {"type": "object", "properties": map[string]any{"runtimeInfo": map[string]any{}}, "required": []string{"runtimeInfo"}}} {
		if optionalRuntimeSchemaAllowed([]*mcp.Tool{{Name: "renew_lease", InputSchema: schema}}, "runtimeInfo") {
			t.Fatal("incompatible schema accepted")
		}
	}
}

func TestRuntimeInfoClientHeartbeatIntegration(t *testing.T) {
	up := newSwitchableUpstream(t)
	f := newDurableFixtureWithPolicy(t, up, t.TempDir(), false, func(o *durableOptions, _ *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) {
		o.activityFactory = func(localactivity.Config) activityObserver {
			return observerFunc(func(context.Context, localactivity.Binding) localactivity.Observation {
				return localactivity.Observation{Model: "claude-opus-5-5", Effort: "xhigh", State: localactivity.Unknown, Reason: "unsupported", ObservedAt: time.Now().UTC()}
			})
		}
	})
	registered, result := f.attach(t, "model-heartbeat")
	if result.IsError {
		t.Fatal("attach failed")
	}
	waitFor(t, "runtime info in broker memory", func() bool {
		info := up.service.SessionRuntimeInfo()[registered.RegistrationID]
		return info.Model == "claude-opus-5-5" && info.Effort == "xhigh"
	})
	f.stop()
}

func TestRuntimeInfoHTTPConnectionSchemaCannotBeInherited(t *testing.T) {
	up := newSwitchableUpstream(t)
	central := httptest.NewServer(mcpserver.Handler(mcpserver.New(up.service, noMemory{})))
	defer central.Close()
	remote := newTestBrokerUpstream(t.Context(), central.URL+"/mcp")
	defer remote.Close()
	req := broker.RegisterRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 7, CWD: t.TempDir(), ClientID: "model-http", ChannelID: "model-http-lease", Secret: strings.Repeat("s", 40), Name: "model-http", RequestID: "model-http-request"}
	var registered broker.RegisterResult
	if err := call(t.Context(), remote, "register_session", req, &registered); err != nil || registered.RuntimeInfoVersion != 1 {
		t.Fatal("register failed")
	}
	raw, _ := json.Marshal(broker.RuntimeInfo{Model: "fixture-model", Effort: "high", Sequence: 1, Generation: registered.Session.Generation, ObservedAt: time.Now().UTC()})
	renew := broker.RenewRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: req.ChannelID, RuntimeInfo: raw}
	var out broker.RegisterResult
	if err := call(t.Context(), remote, "renew_lease", renew, &out); err != nil {
		t.Fatal(err)
	}
	if len(up.service.SessionRuntimeInfo()) != 0 {
		t.Fatal("unverified schema sent runtime info")
	}
	if !remote.verifyActivity(t.Context()) || !remote.runtimeInfoAllowed() {
		t.Fatal("runtime schema not recognized")
	}
	if err := call(t.Context(), remote, "renew_lease", renew, &out); err != nil {
		t.Fatal(err)
	}
	if len(up.service.SessionRuntimeInfo()) != 1 {
		t.Fatal("verified schema lost runtime info")
	}
	remote.mu.Lock()
	old := remote.current
	remote.current = nil
	remote.mu.Unlock()
	_ = old.Close()
	if remote.runtimeInfoAllowed() {
		t.Fatal("replacement inherited schema")
	}
	if err := call(t.Context(), remote, "renew_lease", renew, &out); err != nil {
		t.Fatal(err)
	}
	if len(up.service.SessionRuntimeInfo()) != 0 {
		t.Fatal("replacement sent field before verification")
	}
}
