package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"sync"
	"testing"
	"time"
)

// renameFixture registers one conversation as "before" through the device flow.
type renameFixture struct {
	*deviceModeFixture
	upstream *switchableUpstream
	thread   string
	original broker.Session
}

func newRenameFixture(t *testing.T, harness, dir string, auth *deviceAuthServer, upstream *switchableUpstream) renameFixture {
	t.Helper()
	ids := singleIdentity()
	thread := "11111111-1111-4111-8111-111111111111"
	id := ids[thread]
	id.Harness = harness
	ids[thread] = id
	f := newDeviceModeFixture(t, auth, upstream, dir, ids)
	waitFor(t, "approval requested", func() bool { return f.codeRequests() == 1 })
	f.grant(thread, "asb_s_rename", "before")
	waitFor(t, "delivery ready", func() bool {
		s, err := upstream.session("before")
		return err == nil && s.AutomaticDeliveryReady
	})
	original, err := upstream.session("before")
	if err != nil {
		t.Fatal(err)
	}
	return renameFixture{deviceModeFixture: f, upstream: upstream, thread: thread, original: original}
}

func (f renameFixture) send(to, body string) {
	f.t.Helper()
	if err := call(context.Background(), f.upstream, "send_message", broker.Message{From: to, To: to, Body: body}, nil); err != nil {
		f.t.Fatal(err)
	}
}

func (f renameFixture) rename(name string) {
	f.t.Helper()
	if result := f.call("", "rename_session", map[string]string{"name": name}); result.IsError {
		f.t.Fatalf("rename to %s failed: %+v", name, result)
	}
}

// receive expects exactly these envelopes, in order, with the name each was
// accepted under.
func receiveEnvelopes(t *testing.T, sink chan broker.Message, want ...broker.Message) {
	t.Helper()
	for i, w := range want {
		select {
		case m := <-sink:
			if m.To != w.To || m.Body != w.Body {
				t.Fatalf("envelope %d = %s to %s, want %s to %s", i, m.Body, m.To, w.Body, w.To)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("envelope %d (%s to %s) not delivered", i, w.Body, w.To)
		}
	}
}

// fillReceiver sends twelve envelopes to name: the sink buffers eight unread,
// the receiver holds the ninth on its way in and three stay queued.
func (f renameFixture) fillReceiver(name string) []broker.Message {
	f.t.Helper()
	sent := []broker.Message{}
	for i := 1; i <= 12; i++ {
		body := fmt.Sprintf("%s-%d", name, i)
		f.send(name, body)
		sent = append(sent, broker.Message{To: name, Body: body})
	}
	waitFor(f.t, "three envelopes queued behind one in flight", func() bool {
		s, err := f.upstream.session(name)
		return err == nil && s.PendingMessages == 3
	})
	return sent
}

// An envelope keeps the name it was accepted under. Renaming before the answer
// arrives, or any number of times while envelopes wait in the queue or on
// their way in, delivers every one on the same lease.
func TestDeviceClientRenameDeliversEnvelopesAcrossTheChange(t *testing.T) {
	for _, harness := range []string{"codex", "claude"} {
		t.Run(harness, func(t *testing.T) {
			f := newRenameFixture(t, harness, t.TempDir(), newDeviceAuthServer(t), newSwitchableUpstream(t))
			// The broker renames but the answer is lost, so the client still
			// knows the old name when an envelope for the new one arrives.
			f.upstream.loseNextAnswer("rename_session")
			if lost := f.call("", "rename_session", map[string]string{"name": "after"}); !lost.IsError {
				t.Fatal("lost reply reported success")
			}
			f.send("after", "unanswered")
			receiveEnvelopes(t, f.sinks[f.thread], broker.Message{To: "after", Body: "unanswered"})
			f.rename("after")
			waiting := f.fillReceiver("after")
			for i := 1; i <= 33; i++ {
				f.rename(fmt.Sprintf("n%d", i))
			}
			f.send("n33", "renamed")
			receiveEnvelopes(t, f.sinks[f.thread], append(waiting, broker.Message{To: "n33", Body: "renamed"})...)
			changed, err := f.upstream.session("n33")
			if err != nil || changed.RegistrationID != f.original.RegistrationID || changed.Generation != f.original.Generation || changed.Status != broker.StatusAlive || !changed.AutomaticDeliveryReady || changed.PendingMessages != 0 {
				t.Fatalf("lease not preserved: before %+v after %+v %v", f.original, changed, err)
			}
		})
	}
}

// A client that stops without reporting its receiver lost resumes the same
// registration after a restart; envelopes queued under a name it held before
// the rename still belong to it.
func TestDeviceClientRestartDeliversEnvelopesQueuedBeforeRename(t *testing.T) {
	for _, harness := range []string{"codex", "claude"} {
		t.Run(harness, func(t *testing.T) {
			auth, upstream, dir := newDeviceAuthServer(t), newSwitchableUpstream(t), t.TempDir()
			f := newRenameFixture(t, harness, dir, auth, upstream)
			waiting := f.fillReceiver("before")
			f.rename("after")
			upstream.offline.Store(true)
			receiveEnvelopes(t, f.sinks[f.thread], waiting[:9]...)
			f.stop()
			upstream.offline.Store(false)
			restarted := newDeviceModeFixture(t, auth, upstream, dir, f.identities)
			receiveEnvelopes(t, restarted.sinks[f.thread], waiting[9:]...)
			resumed, err := upstream.session("after")
			if err != nil || resumed.RegistrationID != f.original.RegistrationID || resumed.Status != broker.StatusAlive || !resumed.AutomaticDeliveryReady || resumed.PendingMessages != 0 {
				t.Fatalf("registration not resumed: %+v %v", resumed, err)
			}
		})
	}
}

// scriptedReceiveUpstream keeps the lease alive, answers the receive stream
// with its envelopes in order and then fails like a dropped connection.
type scriptedReceiveUpstream struct{ envelopes []broker.Message }

func (u *scriptedReceiveUpstream) CallTool(_ context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name != "wait_for_message" {
		return &mcp.CallToolResult{StructuredContent: broker.RegisterResult{}}, nil
	}
	if len(u.envelopes) == 0 {
		return nil, errors.New("connection reset by peer")
	}
	m := u.envelopes[0]
	u.envelopes = u.envelopes[1:]
	return &mcp.CallToolResult{StructuredContent: m}, nil
}

func TestDurableReceiverOwnsOnlyItsRegistrationEnvelopes(t *testing.T) {
	const ours, other = "AAAAAAAAAAAAAAAAAAAAAAAAAA", "BBBBBBBBBBBBBBBBBBBBBBBBBB"
	for _, tc := range []struct {
		name      string
		envelope  broker.Message
		exit      receiverExit
		delivered int
	}{
		{"this registration under a former name", broker.Message{To: "former", Body: "b", RegistrationID: ours}, exitUpstream, 1},
		{"another registration under the current name", broker.Message{To: "current", Body: "b", RegistrationID: other}, exitTerminal, 0},
		{"older broker, current name", broker.Message{To: "current", Body: "b"}, exitUpstream, 1},
		{"older broker, another name", broker.Message{To: "former", Body: "b"}, exitTerminal, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &ingressBinding{mu: &sync.Mutex{}, identity: broker.AttachRequest{Name: "current", ChannelID: "lease"}, durable: &durableRegistration{registrationID: ours, name: "current"}}
			delivered := 0
			exit := receiveDurableOnce(t.Context(), &scriptedReceiveUpstream{envelopes: []broker.Message{tc.envelope}}, b, durableOptions{heartbeat: time.Hour, timeout: time.Second}, func(context.Context, broker.Message) error {
				delivered++
				return nil
			})
			if exit != tc.exit || delivered != tc.delivered {
				t.Fatalf("exit %v delivered %d, want exit %v delivered %d", exit, delivered, tc.exit, tc.delivered)
			}
		})
	}
}

func TestDeviceClientRenameUsesCurrentConversationAndPersists(t *testing.T) {
	for _, harness := range []string{"codex", "claude"} {
		t.Run(harness, func(t *testing.T) {
			auth := newDeviceAuthServer(t)
			upstream := newSwitchableUpstream(t)
			ids := singleIdentity()
			thread := "11111111-1111-4111-8111-111111111111"
			id := ids[thread]
			id.Harness = harness
			ids[thread] = id
			dir := t.TempDir()
			f := newDeviceModeFixture(t, auth, upstream, dir, ids)
			waitFor(t, "approval requested", func() bool { return f.codeRequests() == 1 })
			if result := f.call("", "rename_session", map[string]string{"name": "after"}); !result.IsError {
				t.Fatal("rename before approval succeeded")
			}
			f.grant(thread, "asb_s_rename", "before")
			waitFor(t, "registered", func() bool { return f.status("")["registered"] == true })
			original, err := upstream.session("before")
			if err != nil {
				t.Fatal(err)
			}
			upstream.loseNextAnswer("rename_session")
			if lost := f.call("", "rename_session", map[string]string{"name": "after"}); !lost.IsError {
				t.Fatal("lost reply reported success")
			}
			result := f.call("", "rename_session", map[string]string{"name": "after"})
			if result.IsError {
				t.Fatalf("rename failed: %+v", result)
			}
			changed, err := upstream.session("after")
			if err != nil || changed.RegistrationID != original.RegistrationID || changed.Generation != original.Generation {
				t.Fatalf("binding changed: %+v %v", changed, err)
			}
			if f.status("")["name"] != "after" {
				t.Fatal("auth status reports stale name")
			}
			if err := call(context.Background(), upstream, "send_message", broker.Message{From: "after", To: "after", Body: "renamed"}, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case m := <-f.sinks[thread]:
				if m.Body != "renamed" {
					t.Fatal(m)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("receiver stopped after rename")
			}
			f.stop()
			state, err := clientstate.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			clientID, err := state.ClientID()
			if err != nil {
				t.Fatal(err)
			}
			key := clientstate.SessionKey("https://broker/mcp?project=brk", harness, thread, clientID)
			saved, ok, err := state.LoadSession(key)
			if err != nil || !ok || saved.Credential.Nickname != "after" || saved.Registration.Name != "after" {
				t.Fatal("nickname not saved")
			}
			f2 := newDeviceModeFixture(t, auth, upstream, dir, ids)
			waitFor(t, "reconnected with new name", func() bool { return f2.status("")["registered"] == true })
			if f2.status("")["name"] != "after" {
				t.Fatal("restart reverted nickname")
			}
			listed, err := f2.local.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, tool := range listed.Tools {
				if tool.Name == "rename_session" {
					data, _ := json.Marshal(tool.InputSchema)
					var schema struct{ Properties map[string]any }
					_ = json.Unmarshal(data, &schema)
					if len(schema.Properties) != 1 || schema.Properties["name"] == nil {
						t.Fatalf("model can select another registration: %s", data)
					}
				}
			}
		})
	}
}
