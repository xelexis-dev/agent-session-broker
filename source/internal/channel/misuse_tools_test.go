package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func misuseToolDenied(t *testing.T, c *mcp.ClientSession, name string, args any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result, err := c.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err == nil && !result.IsError {
		t.Fatalf("%s accepted foreign or malformed input", name)
	}
	if ctx.Err() != nil {
		t.Fatalf("%s blocked instead of rejecting foreign input", name)
	}
}

func TestMisuseA2ClientRejectsForeignSenderNameAndID(t *testing.T) {
	r := newDetachedRuntime(t)
	a := r.harness(t, t.TempDir(), 9951)
	b := r.harness(t, t.TempDir(), 9952)
	a.attach(t)
	b.attach(t)
	for _, from := range []string{b.session.Name, b.session.ID, b.session.RegistrationID, "registration:" + b.session.RegistrationID} {
		misuseToolDenied(t, a.client, "send_message", map[string]any{"from": from, "to": b.session.Name, "body": "forged"})
	}
	var sent broker.SendResult
	if err := call(t.Context(), a.client, "send_message", map[string]any{"from": a.session.Name, "to": b.session.Name, "body": "authentic"}, &sent); err != nil || !sent.Accepted {
		t.Fatal("valid sender no longer works")
	}
	select {
	case got := <-b.received:
		if got.Body != "authentic" || got.From != a.session.Name {
			t.Fatal("foreign sender was delivered")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("valid message not delivered")
	}
}

func TestMisuseA5ConcurrentExplicitAttachKeepsOneRegistration(t *testing.T) {
	r := newDetachedRuntime(t)
	made := make(chan *ingressBinding, 1)
	var traceMu sync.Mutex
	var trace []string
	h := r.harness(t, t.TempDir(), 9953, func(o *durableOptions) {
		o.bindingCreated = func(b *ingressBinding) { made <- b }
		original := o.device.newUpstream
		o.device.newUpstream = func(token string) Upstream {
			return &explicitReadBarrier{Upstream: original(token), before: func(context.Context, *mcp.CallToolParams) {}, after: func(_ context.Context, p *mcp.CallToolParams, result *mcp.CallToolResult, err error) {
				if p.Name != "get_session" && p.Name != "register_session" && p.Name != "resume_session" && p.Name != "renew_lease" {
					return
				}
				class := "ok"
				if err != nil {
					class = "transport"
					if errors.Is(err, mcp.ErrConnectionClosed) {
						class = "connection-closed"
					}
					if errors.Is(err, context.Canceled) {
						class = "cancelled"
					}
				}
				if result != nil && result.IsError {
					class = "rejected"
					for _, c := range result.Content {
						if c, ok := c.(*mcp.TextContent); ok {
							for _, known := range []string{"session not found", "detached", "registration-in-use", "channel", "credential-rejected"} {
								if strings.Contains(c.Text, known) {
									class = known
									break
								}
							}
						}
					}
				}
				traceMu.Lock()
				trace = append(trace, p.Name+":"+class)
				traceMu.Unlock()
			}}
		}
	})
	var binding *ingressBinding
	snapshot := func(stage string) string {
		if binding == nil {
			binding = <-made
		}
		binding.mu.Lock()
		gen, started, failed := binding.generation, binding.started, binding.failed
		binding.mu.Unlock()
		a := binding.durable.session
		a.mu.Lock()
		permits, recovering, detached := a.explicitAttaching, a.recovering, a.state.Registration.Detached
		a.mu.Unlock()
		traceMu.Lock()
		events := append([]string(nil), trace...)
		traceMu.Unlock()
		return fmt.Sprintf("stage=%s gen=%d started=%t failed=%t permits=%d recovering=%t detached=%t trace=%v", stage, gen, started, failed, permits, recovering, detached, events)
	}
	attach := func(stage string) {
		result, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
		if err != nil || result.IsError {
			class := "transport"
			if result != nil {
				class = "other-rejection"
				for _, c := range result.Content {
					if c, ok := c.(*mcp.TextContent); ok {
						for _, known := range []string{"broker get_session transport failed", "ingress binding changed", "ingress attachment identity mismatch", "ingress registration was retired", "credential-rejected"} {
							if strings.Contains(c.Text, known) {
								class = known
								break
							}
						}
					}
				}
			}
			t.Fatalf("A5 explicit internalClass=%s %s", class, snapshot(stage))
		}
		raw, _ := json.Marshal(result.StructuredContent)
		if err := json.Unmarshal(raw, &h.session); err != nil {
			t.Fatal("A5 invalid success response")
		}
	}
	var group sync.WaitGroup
	failures := make(chan bool, 2)
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
			failures <- err != nil || result.IsError
		}()
	}
	group.Wait()
	close(failures)
	for failed := range failures {
		if failed {
			t.Fatalf("concurrent explicit attach failed %s", snapshot("concurrent"))
		}
	}
	first := h.status(t)
	attach("repeat")
	if h.status(t).RegistrationID != first.RegistrationID || r.direct.Load() != 1 {
		t.Fatal("attach duplicated registration or admission")
	}
	for _, args := range []any{map[string]any{"name": "\ninvalid"}, map[string]any{"sessionId": "22222222-2222-4222-8222-222222222222"}} {
		misuseToolDenied(t, h.client, "attach_session", args)
	}
	// Self detach retains durable identity; explicit reattach must revive it.
	if err := call(t.Context(), h.client, "stop_session", map[string]any{"name": h.session.Name}, nil); err != nil {
		t.Fatal("self stop failed")
	}
	attach("after-stop")
	if h.session.RegistrationID != first.RegistrationID {
		t.Fatal("explicit reattach replaced durable registration")
	}
}

func TestMisuseA7ClientInternalToolsCannotSelectForeignRegistration(t *testing.T) {
	r := newDetachedRuntime(t)
	a := r.harness(t, t.TempDir(), 9954)
	b := r.harness(t, t.TempDir(), 9955)
	a.attach(t)
	b.attach(t)
	foreign := map[string]any{"registrationId": b.session.RegistrationID, "channelId": "foreign-lease", "secret": "foreign-secret", "recipient": b.session.Name}
	for _, tool := range []string{"rebind_session", "register_session", "resume_session", "renew_lease", "probe_response", "report_ingress_failure", "spawn_session", "wait_for_message"} {
		args := any(foreign)
		if tool == "wait_for_message" {
			args = map[string]any{"recipient": b.session.Name, "channelId": "foreign-lease"}
		}

		misuseToolDenied(t, a.client, tool, args)
	}
	var after broker.Session
	if err := call(t.Context(), a.client, "get_session", map[string]any{"name": b.session.Name}, &after); err != nil || after.Revision != b.session.Revision || !after.AutomaticDeliveryReady {
		t.Fatal("internal tool misuse changed foreign session")
	}
}

// Exercise the real server authorization boundary, even with the victim's exact
// lease and resume secret (stronger than model-visible input).
func TestMisuseA7ServerRejectsForeignLeaseEvenWithExactProof(t *testing.T) {
	r := newDetachedRuntime(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	a := r.harness(t, dirA, 9956)
	leaseSpy := &misuseLeaseSpy{}
	b := r.harness(t, dirB, 9957, func(opts *durableOptions) {
		makeUpstream := opts.device.newUpstream
		opts.device.newUpstream = func(token string) Upstream { leaseSpy.Upstream = makeUpstream(token); return leaseSpy }
	})
	a.attach(t)
	b.attach(t)
	load := func(dir string, n int) clientstate.SessionState {
		state, err := clientstate.Open(dir)
		if err != nil {
			t.Fatal("fixture state open failed")
		}
		id, err := state.ClientID()
		if err != nil {
			t.Fatal("fixture client ID unavailable")
		}
		key := clientstate.SessionKey(brokerScope(r.endpoint, "detach"), "codex", fmt.Sprintf("11111111-1111-4111-8111-%012d", n), id)
		saved, ok, err := state.LoadSession(key)
		if err != nil || !ok {
			t.Fatal("fixture registration unavailable")
		}
		return saved
	}
	own, victim := load(dirA, 9956), load(dirB, 9957)
	raw := newTestBrokerUpstream(t.Context(), r.endpoint)
	defer raw.Close()
	raw.setBearerToken(own.Credential.AccessToken)
	leaseSpy.mu.Lock()
	lease := leaseSpy.lease
	leaseSpy.mu.Unlock()
	if lease == "" {
		t.Fatal("victim lease not observed")
	}
	id := b.session.RegistrationID
	args := []struct {
		name  string
		value any
	}{
		{"register_session", broker.RegisterRequest{Harness: "codex", SessionID: b.session.ID, PID: 9957, CWD: "/synthetic/detached", ClientID: victim.ClientID, Name: b.session.Name, Secret: victim.Registration.Secret, ChannelID: lease, RequestID: "forged-register", ExpectedRevision: b.session.Revision}},
		{"renew_lease", broker.RenewRequest{RegistrationID: id, ChannelID: lease}},
		{"probe_response", broker.ProbeAnswer{RegistrationID: id, ChannelID: lease, ProbeID: "foreign-probe", Revision: b.session.Revision, OK: false}},
		{"report_ingress_failure", map[string]any{"registrationId": id, "channelId": lease}},
		{"wait_for_message", map[string]any{"recipient": id, "channelId": lease}},
		{"resume_session", broker.ResumeRequest{RegistrationID: id, Harness: "codex", SessionID: b.session.ID, ClientID: victim.ClientID, PID: 9957, CWD: "/synthetic/detached", Secret: victim.Registration.Secret, ChannelID: "replacement-lease", RequestID: "forged-resume", ExpectedRevision: b.session.Revision}},
		{"rebind_session", broker.RebindRequest{RegistrationID: id, Harness: "codex", SessionID: a.session.ID, ClientID: victim.ClientID, PID: 9956, CWD: "/synthetic/detached", Secret: victim.Registration.Secret, RequestID: "forged-rebind", ExpectedRevision: b.session.Revision}},
	}
	for _, tc := range args {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		result, err := raw.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: tc.value})
		timedOut := ctx.Err() != nil
		cancel()
		if timedOut {
			t.Fatalf("%s blocked instead of rejecting foreign lease", tc.name)
		}
		if err == nil && !result.IsError {
			encoded, _ := json.Marshal(result.StructuredContent)
			var refusal struct{ Code, Reason string }
			_ = json.Unmarshal(encoded, &refusal)
			if refusal.Reason == "" && refusal.Code != broker.CodeInvalidToken && refusal.Code != broker.CodeOwned {
				t.Fatalf("%s did not reject at the credential ownership boundary", tc.name)
			}
		}
	}
	var current broker.Session
	if err := call(t.Context(), a.client, "get_session", map[string]any{"name": b.session.Name}, &current); err != nil || current.Revision != b.session.Revision || !current.AutomaticDeliveryReady {
		t.Fatal("foreign proofs changed victim")
	}
}

// Capture only synthetic test protocol fields, never log credentials or proofs.
type misuseLeaseSpy struct {
	Upstream
	mu    sync.Mutex
	lease string
}

func (s *misuseLeaseSpy) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name == "register_session" || p.Name == "resume_session" || p.Name == "renew_lease" {
		raw, _ := json.Marshal(p.Arguments)
		var in struct{ ChannelID string }
		_ = json.Unmarshal(raw, &in)
		if in.ChannelID != "" {
			s.mu.Lock()
			s.lease = in.ChannelID
			s.mu.Unlock()
		}
	}
	return s.Upstream.CallTool(ctx, p)
}

func TestMisuseA8ClientForwardingRejectsMalformedInputWithoutEcho(t *testing.T) {
	r := newDetachedRuntime(t)
	h := r.harness(t, t.TempDir(), 9958)
	h.attach(t)
	marker := strings.Join([]string{"asb", "p", "synthetic-invalid-field"}, "_")
	for _, tc := range []struct {
		name string
		args any
	}{
		{"send_message", map[string]any{"from": h.session.Name, "to": h.session.Name, "body": 7}},
		{"send_message", map[string]any{"from": h.session.Name, "to": h.session.Name, "body": "valid", "unknown": marker}},
		{"send_message", map[string]any{"from": h.session.Name, "to": h.session.Name, "body": strings.Repeat("x", 1024*1024)}},
		{"send_message", json.RawMessage(`null`)},
		{"send_message", map[string]any{}},
		{"unknown_tool", map[string]any{}},
		{"report_bug", map[string]any{"title": "fixture", "summary": "fixture", "severity": "urgent"}},
		{"report_bug", map[string]any{"title": strings.Repeat("가", 201), "summary": "fixture"}},
	} {
		result, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
		if err == nil && !result.IsError {
			t.Fatalf("%s accepted malformed input", tc.name)
		}
		raw, _ := json.Marshal(result)
		if strings.Contains(string(raw), marker) || (err != nil && strings.Contains(err.Error(), marker)) {
			t.Fatal("invalid input echoed token-shaped unknown field")
		}
	}
	if status := h.status(t); !status.Registered || status.RegistrationID != h.session.RegistrationID {
		t.Fatal("malformed forwarding broke registration")
	}
}

func (s *misuseLeaseSpy) Close() error { closeUpstream(s.Upstream); return nil }
