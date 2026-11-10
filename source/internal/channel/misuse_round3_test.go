package channel

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRound3HolderDeathDuringWaitAndFormerHolderRestart(t *testing.T) {
	u := newSwitchableUpstream(t)
	id := claudeHandoverIdentity(1850, 21850)
	owner := fixtureHolder()
	owner.PID = 101
	req := broker.RegisterRequest{Holder: owner, Harness: "claude", SessionID: id.SessionID, ClientID: "copied-client", PID: id.PID, CWD: id.CWD, Name: "copied-owner", Secret: clientstate.NewSecret(), RequestID: "initial", ChannelID: "lease-original"}
	var registered broker.RegisterResult
	if err := call(t.Context(), u, "register_session", req, &registered); err != nil || registered.Code != "" {
		t.Fatalf("fixture register: err=%v code=%q", err, registered.Code)
	}
	challenger := *owner
	challenger.PID = 102
	receiver := &fixtureReceiverIdentity{holder: &challenger}
	state, _ := clientstate.Open(t.TempDir())
	binding := func() *ingressBinding {
		return &ingressBinding{mu: &sync.Mutex{}, identity: id, durable: &durableRegistration{key: "fixture-key", clientID: req.ClientID, registrationID: registered.Session.RegistrationID, name: req.Name, secret: req.Secret, revision: registered.Session.Revision}}
	}
	b := binding()
	waits := 0
	opts := (durableOptions{state: state, scope: "fixture", receiverIdentity: receiver, resumeClock: devicePollClock{sleep: func(context.Context, time.Duration) error { waits++; receiver.dead = true; return nil }}}).withDefaults()
	resumed, ok := resumeDurable(t.Context(), u, b, opts, "")
	if !ok || waits != 1 || resumed.RegistrationID != registered.Session.RegistrationID || b.holderWaiting {
		t.Fatal("holder death did not converge on next wait without user action")
	}
	// A restarted former receiver has a new incarnation and may not steal back.
	old := *owner
	old.Incarnation = strings.Repeat("a", 64)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts.receiverIdentity = fixtureReceiverIdentity{holder: &old}
	opts.resumeClock.sleep = func(context.Context, time.Duration) error { cancel(); return ctx.Err() }
	if _, ok := resumeDurable(ctx, u, binding(), opts, ""); ok {
		t.Fatal("former holder stole live successor")
	}
	var renewed broker.RegisterResult
	if err := call(t.Context(), u, "renew_lease", broker.RenewRequest{RegistrationID: resumed.RegistrationID, ChannelID: b.identity.ChannelID}, &renewed); err != nil || renewed.Code != "" {
		t.Fatal("successor lease lost after former-holder restart")
	}
}

func TestRound3AuthenticatedDowngradeRestoreAndTerminalReset(t *testing.T) {
	store, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal("fixture store")
	}
	p, err := newProjectTracker(store, "same-endpoint")
	if err != nil {
		t.Fatal("fixture tracker")
	}
	source, parent := "fixture-child", "fixture-admission"
	generation := strings.Repeat("A", 26)
	p.learn(source, parent, generation, true)
	// Emulate authenticated 0.17.1 initialize, which has no G2 capability.
	p.initialized(source, parent, &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{}})
	if p.hint(source) != "" || p.hint(parent) != "" {
		t.Fatal("downgrade retained active generation hint")
	}
	if terminalProjectError(p.refusal(source, 404, []byte(`{"error":"project_not_found"}`))) {
		t.Fatal("downgrade became falsely terminal")
	}
	// Restore the original authenticated generation after an empty-server outage.
	p.learn(source, parent, generation, true)
	if p.hint(source) != generation {
		t.Fatal("restored server could not renegotiate")
	}
	if !terminalProjectError(p.refusal(source, 410, []byte(`{"error":"project_deleted"}`))) {
		t.Fatal("fixture terminal")
	}
	restarted, err := newProjectTracker(store, "same-endpoint")
	if err != nil || !terminalProjectError(restarted.terminal(source)) {
		t.Fatal("restart without action cleared terminal")
	}
	other, _ := newProjectTracker(store, "different-endpoint")
	if other.terminal(source) != nil || other.hint(source) != "" {
		t.Fatal("endpoint change inherited terminal/hint")
	}
	if restarted.terminal("new-token") != nil || restarted.hint("new-token") != "" {
		t.Fatal("token change inherited terminal/hint")
	}
	restarted.reset(source)
	if restarted.terminal(source) != nil || restarted.hint(source) != "" {
		t.Fatal("explicit recovery did not reset terminal/hint")
	}
}
