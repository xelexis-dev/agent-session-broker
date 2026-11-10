package channel

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type returnAuthorizer struct {
	*deviceAuthClient
	fresh *atomic.Int32
}

func (a *returnAuthorizer) exchange(ctx context.Context, r directCodeRequest) (directGrant, error) {
	if !r.Handover {
		a.fresh.Add(1)
	}
	return a.deviceAuthClient.exchange(ctx, r)
}

func TestResumeFollowReturnRestoresRegistrationQueueAndMarkers(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	c0 := claudeHandoverIdentity(401, 6101)
	c1 := claudeHandoverIdentity(402, 6101)
	roster := &followRoster{id: c0}
	var fresh, connections atomic.Int32
	h := r.identityHarness(t, dir, c0, func(o *durableOptions) {
		followOptions(roster)(o)
		o.device.authorizer = &returnAuthorizer{o.device.authorizer.(*deviceAuthClient), &fresh}
		original := o.device.newUpstream
		o.device.newUpstream = func(token string) Upstream {
			u := original(token)
			if connections.Add(1) <= 2 {
				return &handoverBlockedReceiver{u.(*brokerUpstream)}
			}
			return u
		}
	})
	h.attach(t)
	original := h.session
	roster.set(c1)
	waitFor(t, "clear followed", func() bool {
		s := h.status(t)
		return s.Registered && s.RegistrationID == original.RegistrationID && s.HandoverFrom != nil
	})
	store, k0, s0 := handoverSaved(t, r, dir, c0)
	_, k1, _ := handoverSaved(t, r, dir, c1)
	if s0.Registration.HandedOverTo != k1 {
		t.Fatal("clear did not mark old conversation")
	}
	manager := r.harness(t, t.TempDir(), 6102)
	manager.attach(t)
	if err := call(t.Context(), manager.client, "send_message", broker.Message{From: manager.session.Name, To: original.Name, Body: "return-queued"}, nil); err != nil {
		t.Fatal(err)
	}
	roster.set(c0)
	waitFor(t, "original conversation returned without reconnect", func() bool {
		s := h.status(t)
		return s.Registered && s.RegistrationID == original.RegistrationID && s.Name == original.Name && s.ReceiveError == "" && s.HandoverFrom != nil && s.HandoverFrom.Revision > original.Revision+1
	})
	select {
	case m := <-h.received:
		if m.Body != "return-queued" {
			t.Fatal("wrong queued message")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("return lost queue")
	}
	a, _, err := store.LoadSession(k0)
	if err != nil || a.Registration.HandedOverTo != "" {
		t.Fatal("destination marker not cleared")
	}
	b, _, err := store.LoadSession(k1)
	if err != nil || b.Registration.HandedOverTo != k0 {
		t.Fatal("source marker not reversed")
	}
	roster.set(c1)
	waitFor(t, "second clear after return", func() bool {
		s := h.status(t)
		return s.Registered && s.ReceiveError == "" && s.HandoverFrom != nil && s.HandoverFrom.Revision == a.Registration.Revision
	})
	if fresh.Load() != 1 {
		t.Fatal("return allocated fresh registration")
	}
	var sessions []broker.Session
	if err := call(t.Context(), manager.client, "list_sessions", map[string]any{}, &sessions); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, s := range sessions {
		if s.Harness == "claude" {
			count++
			if s.RegistrationID != original.RegistrationID {
				t.Fatal("registration changed")
			}
		}
	}
	if count != 1 {
		t.Fatal("multiple registrations after return")
	}
}

func TestResumeFollowReturnStartupFindsHolderWithoutFreshAdmission(t *testing.T) {
	for _, kind := range []string{"inactive", "other-pid-alive", "cycle", "overlong", "detached"} {
		t.Run(kind, func(t *testing.T) {
			r := newDetachedRuntime(t)
			dir := t.TempDir()
			c0 := claudeHandoverIdentity(410, 6201)
			c1 := claudeHandoverIdentity(411, 6201)
			old := r.identityHarness(t, dir, c0, handoverOptions)
			old.attach(t)
			original := old.session
			old.close()
			holder := r.identityHarness(t, dir, c1, handoverOptions)
			holder.attach(t)
			store, k0, state0 := handoverSaved(t, r, dir, c0)
			_, k1, state1 := handoverSaved(t, r, dir, c1)
			observed := c0
			if kind == "other-pid-alive" {
				observed.PID++
			} else {
				holder.close()
			}
			switch kind {
			case "cycle":
				if err := store.MarkHandedOver(k1, k0); err != nil {
					t.Fatal(err)
				}
			case "overlong":
				key := k1
				for n := 0; n < 8; n++ {
					s := state1
					s.SessionID = claudeHandoverIdentity(420+n, 6201).SessionID
					s.Registration.HandedOverTo = ""
					next := clientstate.SessionKey(s.Scope, s.Harness, s.SessionID, s.ClientID)
					if err := store.SaveSession(next, s); err != nil {
						t.Fatal(err)
					}
					if err := store.MarkHandedOver(key, next); err != nil {
						t.Fatal(err)
					}
					key = next
				}
			case "detached":
				state0.Registration.Detached = true
				if err := store.SaveSession(k0, state0); err != nil {
					t.Fatal(err)
				}
			}
			var fresh atomic.Int32
			before := r.direct.Load()
			roster := &followRoster{id: observed}
			h := r.identityHarness(t, dir, observed, func(o *durableOptions) {
				followOptions(roster)(o)
				o.device.authorizer = &returnAuthorizer{o.device.authorizer.(*deviceAuthClient), &fresh}
			})
			if kind == "inactive" {
				waitFor(t, "startup automatically recovered holder", func() bool {
					s := h.status(t)
					return s.Registered && s.RegistrationID == original.RegistrationID && s.Name == original.Name
				})
				state, _, err := store.LoadSession(k0)
				if err != nil || state.Registration.HandedOverTo != "" {
					t.Fatal("startup did not clear destination marker")
				}
			} else if kind == "detached" {
				if h.status(t).Registered {
					t.Fatal("detached startup registered")
				}
			} else {
				reason := "refused"
				if kind == "other-pid-alive" {
					reason = "still-receiving"
				}
				waitFor(t, "startup refusal diagnostic", func() bool {
					s := h.status(t)
					return !s.Registered && s.Diagnostics != nil && s.Diagnostics.HandoverSkipped == reason
				})
			}
			if fresh.Load() != 0 {
				t.Fatal("startup return created fresh registration")
			}
			if kind == "cycle" || kind == "overlong" || kind == "detached" {
				if r.direct.Load() != before {
					t.Fatal("invalid chain started direct exchange")
				}
			}
		})
	}
}

func TestResumeFollowReturnDoesNotStealUnrelatedHolder(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	b := claudeHandoverIdentity(440, 6301)
	holderID := claudeHandoverIdentity(441, 6301)
	first := r.identityHarness(t, dir, b, handoverOptions)
	first.attach(t)
	foreignReg := first.session
	first.close()
	holder := r.identityHarness(t, dir, holderID, handoverOptions)
	holder.attach(t)
	a := claudeHandoverIdentity(442, 6302)
	roster := &followRoster{id: a}
	current := r.identityHarness(t, dir, a, followOptions(roster))
	current.attach(t)
	ownReg := current.session
	if ownReg.RegistrationID == foreignReg.RegistrationID {
		t.Fatal("setup stole live foreign holder")
	}
	returned := b
	returned.PID = a.PID
	roster.set(returned)
	waitFor(t, "own registration moved to returned conversation", func() bool {
		s := current.status(t)
		return s.Registered && s.RegistrationID == ownReg.RegistrationID && s.ReceiveError == ""
	})
	var remote broker.Session
	if err := call(t.Context(), current.client, "get_session", map[string]string{"name": foreignReg.RegistrationID}, &remote); err != nil || remote.ID != holderID.SessionID || remote.Name != foreignReg.Name || !remote.AutomaticDeliveryReady {
		t.Fatal("unrelated live holder was stolen")
	}
	_, _, state := handoverSaved(t, r, dir, holderID)
	if state.Registration.HandedOverTo != "" {
		t.Fatal("unrelated holder was marked handed over")
	}
}
