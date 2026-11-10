package channel

import (
	"testing"

	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestSelfStopJoinsReceiverBeforeReturning(t *testing.T) {
	r := newDetachedRuntime(t)
	var binding *ingressBinding
	h := r.harness(t, t.TempDir(), 9961, func(o *durableOptions) { o.bindingCreated = func(b *ingressBinding) { binding = b } })
	h.attach(t)
	original := h.session.RegistrationID
	if err := call(t.Context(), h.client, "stop_session", map[string]string{"name": h.session.Name}, nil); err != nil {
		t.Fatal(err)
	}
	binding.mu.Lock()
	retired, detached, done := binding.retired, binding.durable.detached, binding.done
	binding.mu.Unlock()
	if !retired || !detached {
		t.Fatal("successful self-stop returned before the local receiver was retired and detach persisted")
	}
	select {
	case <-done:
	default:
		t.Fatal("self-stop returned before receiver exit")
	}
	h.attach(t)
	if h.session.RegistrationID != original || h.session.Status != broker.StatusAlive || !h.session.AutomaticDeliveryReady {
		t.Fatal("explicit reattach failed to revive the same registration")
	}
}

func TestStoppingAnotherSessionKeepsCallerReceiver(t *testing.T) {
	r := newDetachedRuntime(t)
	var own *ingressBinding
	caller := r.harness(t, t.TempDir(), 9962, func(o *durableOptions) { o.bindingCreated = func(b *ingressBinding) { own = b } })
	other := r.harness(t, t.TempDir(), 9963)
	caller.attach(t)
	other.attach(t)
	if err := call(t.Context(), caller.client, "stop_session", map[string]string{"name": other.session.Name}, nil); err != nil {
		t.Fatal(err)
	}
	own.mu.Lock()
	retired, done := own.retired, own.done
	own.mu.Unlock()
	if retired {
		t.Fatal("stopping another session retired caller")
	}
	select {
	case <-done:
		t.Fatal("caller receiver stopped")
	default:
	}
	if !caller.status(t).Registered {
		t.Fatal("caller registration lost")
	}
}
