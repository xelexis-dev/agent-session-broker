package channel

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"os"
	"path/filepath"
	"testing"
)

func TestDeliveryPruneEpochTransition(t *testing.T) {
	old, b, opts := messageAckFixture(t, nil, "old-epoch", "lease")
	if err := opts.state.SaveDeliveryWatermark(opts.scope, old.registration, old.epoch, 7); err != nil {
		t.Fatal(err)
	}
	result := broker.RegisterResult{MessageAckVersion: 1, Session: broker.Session{RegistrationID: old.registration, Epoch: "new-epoch"}}
	if err := opts.negotiateMessageAck(b, result, "new-lease", []string{broker.MessageAckCapability}); err != nil {
		t.Fatal(err)
	}
	if got, err := opts.state.LoadDeliveryWatermark(opts.scope, old.registration, old.epoch); err != nil || got != 0 {
		t.Fatalf("old epoch watermark retained: floor=%d error=%v", got, err)
	}
}

func TestDeliveryPruneTransitionKeepsUnrelated(t *testing.T) {
	for _, kind := range []string{"same-epoch", "other-registration", "other-scope", "failed-negotiation"} {
		t.Run(kind, func(t *testing.T) {
			old, b, opts := messageAckFixture(t, nil, "old-epoch", "lease")
			if err := opts.state.SaveDeliveryWatermark(opts.scope, old.registration, old.epoch, 7); err != nil {
				t.Fatal(err)
			}
			originalScope := opts.scope
			result := broker.RegisterResult{MessageAckVersion: 1, Session: broker.Session{RegistrationID: old.registration, Epoch: "new-epoch"}}
			switch kind {
			case "same-epoch":
				result.Session.Epoch = old.epoch
			case "other-registration":
				result.Session.RegistrationID = "BBBBBBBBBBBBBBBBBBBBBBBBBB"
			case "other-scope":
				opts.scope = "different-scope"
			case "failed-negotiation":
				result.MessageAckVersion = 2
			}
			err := opts.negotiateMessageAck(b, result, "next-lease", []string{broker.MessageAckCapability})
			if (err != nil) != (kind == "failed-negotiation") {
				t.Fatal("unexpected negotiation result")
			}
			if got, err := opts.state.LoadDeliveryWatermark(originalScope, old.registration, old.epoch); err != nil || got != 7 {
				t.Fatal("unproven floor removed")
			}
		})
	}
}

func TestDeliveryPruneResumeTerminalAnswers(t *testing.T) {
	for _, code := range []string{broker.CodeNotFound, broker.CodeDetached, broker.CodeOwned, broker.CodeInvalid, "handover"} {
		t.Run(code, func(t *testing.T) {
			old, b, opts := messageAckFixture(t, nil, "epoch", "lease")
			b.durable = &durableRegistration{registrationID: old.registration, key: clientstate.RegistrationKey(opts.scope, "claude", "11111111-1111-4111-8111-111111111111")}
			if err := opts.state.SaveDeliveryWatermark(opts.scope, old.registration, old.epoch, 7); err != nil {
				t.Fatal(err)
			}
			up := &clockedResumeUpstream{brokerUpstream: &brokerUpstream{}, respond: func() (broker.RegisterResult, error) {
				// A late detached answer follows a same-epoch reattach writer's newer floor.
				if code == broker.CodeDetached {
					if err := opts.state.SaveDeliveryWatermark(opts.scope, old.registration, old.epoch, 9); err != nil {
						t.Fatal(err)
					}
				}
				return broker.RegisterResult{Code: code}, nil
			}}
			if _, ok := resumeDurable(t.Context(), up, b, opts, ""); ok {
				t.Fatal("terminal answer resumed")
			}
			want := uint64(7)
			if code == broker.CodeNotFound {
				want = 0
			}
			if code == broker.CodeDetached {
				want = 9
			}
			if got, err := opts.state.LoadDeliveryWatermark(opts.scope, old.registration, old.epoch); err != nil || got != want {
				t.Fatalf("floor=%d want=%d error=%v", got, want, err)
			}
		})
	}
}

func TestDeliveryPruneNotFoundScopeFence(t *testing.T) {
	for _, kind := range []string{"registration", "scope"} {
		t.Run(kind, func(t *testing.T) {
			old, b, opts := messageAckFixture(t, nil, "epoch", "lease")
			scope := opts.scope
			registration := old.registration
			if err := opts.state.SaveDeliveryWatermark(scope, registration, old.epoch, 7); err != nil {
				t.Fatal(err)
			}
			if kind == "scope" {
				opts.scope = "other"
			} else {
				registration = "other"
			}
			opts.pruneMessageAck(b, registration, broker.CodeNotFound)
			if got, err := opts.state.LoadDeliveryWatermark(scope, old.registration, old.epoch); err != nil || got != 7 {
				t.Fatal("wrong namespace deleted")
			}
		})
	}
}

func TestDeliveryPruneFailureDoesNotBlockNegotiation(t *testing.T) {
	old, b, opts := messageAckFixture(t, nil, "epoch", "lease")
	// Nonregular obsolete state is skipped rather than affecting the new receiver.
	if err := os.MkdirAll(filepath.Join(opts.state.Dir(), "delivery"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := opts.state.SaveDeliveryWatermark(opts.scope, old.registration, old.epoch, 7); err != nil {
		t.Fatal(err)
	}
	result := broker.RegisterResult{MessageAckVersion: 1, Session: broker.Session{RegistrationID: old.registration, Epoch: "next"}}
	// The helper's busy branch is covered in clientstate; a removed store parent
	// also cannot prevent a new empty epoch from being negotiated.
	if err := os.Rename(filepath.Join(opts.state.Dir(), "delivery"), filepath.Join(opts.state.Dir(), "held")); err != nil {
		t.Fatal(err)
	}
	if err := opts.negotiateMessageAck(b, result, "next", []string{broker.MessageAckCapability}); err != nil {
		t.Fatal(err)
	}
}
