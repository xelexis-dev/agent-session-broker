package channel

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// Hold the real rejection exit path open until approval's automatic attempt
// returns. Removing the post-exit retry must strand registration without a tool.
func TestDeviceModeReapprovalWaitsForOldReceiver(t *testing.T) {
	for _, mode := range []string{"resume", "cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) { testReapprovalWait(t, mode) })
	}
}

func testReapprovalWait(t *testing.T, mode string) {
	auth := newDeviceAuthServer(t)
	upstream := newSwitchableUpstream(t)
	rejected := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var capturedMu sync.Mutex
	var bindings []*ingressBinding
	f := newDeviceModeFixture(t, auth, upstream, t.TempDir(), singleIdentity(), func(f *deviceModeFixture) {
		f.configureDurable = func(o *durableOptions) {
			o.bindingCreated = func(b *ingressBinding) { capturedMu.Lock(); bindings = append(bindings, b); capturedMu.Unlock() }
		}
		var rejectOnce sync.Once
		f.persist = func(s *clientstate.Store, key string, state clientstate.SessionState) error {
			err := s.SaveSession(key, state)
			if state.Device != nil && state.Device.Status == clientstate.DeviceRejected {
				rejectOnce.Do(func() { close(rejected); <-release })
			}
			return err
		}
	})
	t.Cleanup(unblock)
	waitFor(t, "initial approval request", func() bool { return f.codeRequests() == 1 })
	const thread = "11111111-1111-4111-8111-111111111111"
	f.grant(thread, "asb_s_old", "agent-one")
	waitFor(t, "initial registration", func() bool { s, e := upstream.session("agent-one"); return e == nil && s.AutomaticDeliveryReady })
	original, _ := upstream.session("agent-one")
	capturedMu.Lock()
	old := bindings[0]
	capturedMu.Unlock()
	var oldDone <-chan struct{}
	waitFor(t, "initial receiver started", func() bool {
		old.mu.Lock()
		defer old.mu.Unlock()
		oldDone = old.done
		return old.started && oldDone != nil
	})
	f.mu.Lock()
	delete(f.grants, "dev-"+thread)
	f.upstreams[0].rejected.Store(true)
	f.mu.Unlock()
	select {
	case <-rejected:
	case <-time.After(3 * time.Second):
		t.Fatal("receiver rejection barrier not reached")
	}
	if result := f.call("", "attach_session", map[string]any{}); !result.IsError {
		t.Fatal("new approval was not requested")
	}
	since := time.Now()
	f.grant(thread, "asb_s_new", "agent-one")
	state, _ := clientstate.Open(f.stateDir)
	key := old.durable.key
	// Written only after the automatic attempt returns; no ordinary tool calls.
	waitFor(t, "approval automatic attempt", func() bool {
		d, e := state.LoadDiagnostics(key)
		if e != nil {
			return false
		}
		for _, event := range d.Events {
			if event.At.After(since) && event.Event == "reconnect-failed" {
				return true
			}
		}
		return false
	})
	select {
	case <-oldDone:
		t.Fatal("old receiver escaped the barrier")
	default:
	}
	if mode != "resume" {
		code := "deadline"
		if mode == "cancel" {
			f.cancel()
			code = "cancelled"
		}
		waitFor(t, "bounded approval wait exit", func() bool {
			d, err := state.LoadDiagnostics(key)
			return err == nil && hasDiagnostic(d, "reconnect-failed", code)
		})
		unblock()
		select {
		case <-oldDone:
		case <-time.After(3 * time.Second):
			t.Fatal("old receiver did not stop after release")
		}
		if f.log.last("resume_session") == "asb_s_new" {
			t.Fatal("automatic registration retried after the wait ended")
		}
		return
	}
	unblock()
	waitFor(t, "automatic same-registration recovery without tool calls", func() bool {
		s, e := upstream.session("agent-one")
		return e == nil && s.RegistrationID == original.RegistrationID && s.Name == original.Name && s.Generation > original.Generation && s.AutomaticDeliveryReady && f.log.last("resume_session") == "asb_s_new"
	})
	var sent broker.Message
	if err := call(context.Background(), upstream, "send_message", map[string]string{"from": "agent-one", "to": "agent-one", "body": "recovered"}, &sent); err != nil {
		t.Fatal("send after recovery failed")
	}
	select {
	case <-f.sinks[thread]:
	case <-time.After(3 * time.Second):
		t.Fatal("replacement receiver did not deliver")
	}
}

// Retain bindings locally for failure-only diagnostics. No snapshot includes
// identity, credential, URL, raw error, approval code or message content.
type reapprovalDiagnostics struct {
	mu                   sync.Mutex
	current              *ingressBinding
	oldDone              <-chan struct{}
	registrationID, name string
}

func (d *reapprovalDiagnostics) bindingCreated(b *ingressBinding) {
	d.mu.Lock()
	d.current = b
	d.mu.Unlock()
}

func (d *reapprovalDiagnostics) rememberOriginal(registrationID, name string) {
	d.mu.Lock()
	d.registrationID, d.name = registrationID, name
	b := d.current
	d.mu.Unlock()
	if b != nil {
		b.mu.Lock()
		done := b.done
		b.mu.Unlock()
		d.mu.Lock()
		d.oldDone = done
		d.mu.Unlock()
	}
}

func (d *reapprovalDiagnostics) report(t *testing.T, state *clientstate.Store) {
	t.Helper()
	if !t.Failed() {
		return
	}
	d.mu.Lock()
	b, done, id, name := d.current, d.oldDone, d.registrationID, d.name
	d.mu.Unlock()
	if b == nil {
		t.Log("reapproval-diagnostics step=reapproval-registration binding_present=false")
		return
	}
	b.mu.Lock()
	started, failed := b.started, b.failed
	idMatch, nameMatch := id != "" && b.durable.registrationID == id, name != "" && b.durable.name == name
	a, key := b.durable.session, b.durable.key
	b.mu.Unlock()
	a.mu.Lock()
	status := a.status
	deviceStatus := "none"
	if a.state.Device != nil {
		switch string(a.state.Device.Status) {
		case "pending", "redeemed", "denied", "expired", "failed", "rejected":
			deviceStatus = string(a.state.Device.Status)
		default:
			deviceStatus = "other"
		}
	}
	a.mu.Unlock()
	switch status {
	case authUnauthorized, authPending, authAuthorized, authDenied, authExpired, authRejected, authFailed:
	default:
		status = "unknown"
	}
	doneClosed := false
	select {
	case <-done:
		doneClosed = true
	default:
	}
	reason, code := "none", "none"
	if out, err := state.LoadDiagnostics(key); err == nil {
		switch out.Summary.LastFailureReason {
		case "renew-failed", "receive-failed", "sink-failed", "reconnect-failed", "probe-failed":
			reason = out.Summary.LastFailureReason
		}
		if out.Summary.LastFailureCode != "" {
			code = diagnosticBrokerReason(out.Summary.LastFailureCode)
		}
	}
	t.Logf("reapproval-diagnostics step=reapproval-registration auth=%s device=%s reason=%s code=%s registered=%t id_match=%t name_match=%t binding_started=%t binding_failed=%t old_receiver_done=%t", status, deviceStatus, reason, code, started && !failed, idMatch, nameMatch, started, failed, doneClosed)
}

func TestRetiredReceiverDoesNotRejectReplacementCredential(t *testing.T) {
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := clientstate.RegistrationKey("scope", "codex", "11111111-1111-4111-8111-111111111111")
	a := &sessionAuth{key: key, status: authAuthorized, state: clientstate.SessionState{Credential: &clientstate.SessionCredential{AccessToken: "replacement"}}}
	o := durableOptions{state: state}
	o.rejectCredentialIfCurrent(a, "retired")
	if a.status != authAuthorized || a.state.Credential == nil {
		t.Fatal("retired receiver rejected replacement approval")
	}
	o.rejectCredentialIfCurrent(a, "replacement")
	if a.status != authRejected || a.state.Credential != nil {
		t.Fatal("current rejection was ignored")
	}
}
