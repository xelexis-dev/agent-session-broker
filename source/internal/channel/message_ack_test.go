package channel

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func (u *switchableUpstream) verifyMessageAck(context.Context) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	info := u.client.InitializeResult()
	return info != nil && info.Capabilities.Experimental[broker.MessageAckCapability] == true
}
func ackClientBinding(t *testing.T, opts durableOptions, id broker.AttachRequest) (*ingressBinding, func()) {
	t.Helper()
	d, unlock, err := opts.loadRegistration(id)
	if err != nil {
		t.Fatal("load fixture registration", err)
	}
	return &ingressBinding{mu: &sync.Mutex{}, identity: id, durable: d}, unlock
}
func TestClientCrashBeforeHostHandoffRedeliversOnce(t *testing.T) {
	up := newSwitchableUpstream(t)
	store, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := (durableOptions{enabled: true, state: store, scope: "http://broker/mcp?project=brk", project: "brk", heartbeat: time.Hour, timeout: time.Second, backoff: func(int) time.Duration { return time.Millisecond }}).withDefaults()
	id := broker.AttachRequest{Harness: "claude", Name: "ack-client", SessionID: "11111111-1111-4111-8111-111111111111", PID: 4242, CWD: "/synthetic", ChannelID: "first-lease"}
	first, unlock := ackClientBinding(t, opts, id)
	registered, err := registerDurable(t.Context(), up, first, opts, id)
	if err != nil {
		t.Fatal("register", err)
	}
	first.identity.Name = registered.Name
	ctx, cancel := context.WithCancel(t.Context())
	dequeued, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		receiveDurableOnce(ctx, up, first, opts, func(ctx context.Context, _ broker.Message) error { close(dequeued); <-ctx.Done(); return ctx.Err() })
	}()
	var sent broker.SendResult
	if err := call(t.Context(), up, "send_message", broker.Message{From: id.Name, To: id.Name, Body: "m1"}, &sent); err != nil {
		cancel()
		t.Fatal(err)
	}
	select {
	case <-dequeued:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("message was not dequeued")
	}
	cancel()
	<-stopped
	unlock()
	// New client process state, with no host handoff completed by the first one.
	second, release := ackClientBinding(t, opts, id)
	defer release()
	if _, ok := resumeDurable(t.Context(), up, second, opts, ""); !ok {
		t.Fatal("new client resume failed")
	}
	resumed, cancelResume := context.WithCancel(t.Context())
	defer cancelResume()
	delivered := make(chan broker.Message, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		receiveDurableOnce(resumed, up, second, opts, func(_ context.Context, m broker.Message) error { delivered <- m; return nil })
	}()
	select {
	case m := <-delivered:
		if m.Body != "m1" || m.DeliverySequence != 1 {
			t.Error("redelivery changed message")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("client lost message after dequeue before host handoff")
	}
	cancelResume()
	<-done
	if len(delivered) != 0 {
		t.Error("host received duplicate in one client")
	}
}
