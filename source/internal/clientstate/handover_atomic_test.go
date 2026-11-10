package clientstate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestHandoverMarkerWriteFailureNeverResolvesRevokedSource(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	destination := savedHandover(t, s, 701)
	source := savedHandover(t, s, 702)
	source.State.Registration.Revision = 3
	if err = s.SaveSession(source.Key, source.State); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkHandedOver(destination.Key, source.Key); err != nil {
		t.Fatal(err)
	}
	// Server has returned the destination's replacement credential. This is the
	// durable state immediately before prepareConversationHandover's marker writes.
	destination.State.Registration.Revision = 4
	destination.State.Registration.HandoverFrom = &protocol.HandoverFrom{Revision: 3}
	destination.State.Credential = &SessionCredential{AccessToken: "synthetic-replacement", AdmissionMode: "auto"}
	if err = s.SaveSession(destination.Key, destination.State); err != nil {
		t.Fatal(err)
	}
	if err = s.ClearHandedOver(destination.Key); err != nil {
		t.Fatal(err)
	}
	// Refuse the second marker write without altering conn.json or its secret.
	if err = os.Mkdir(filepath.Join(s.sessionDir(source.Key), "handover.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkHandedOver(source.Key, destination.Key); err == nil {
		t.Fatal("marker failure fixture did not fail")
	}
	reopened, err := Open(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	holder, err := reopened.HandoverHolder(source.Key, source.State.Scope, source.State.Harness, source.State.ClientID, false)
	if err == nil && holder.Key != destination.Key {
		t.Fatal("marker write failure resolves the revoked source as holder")
	}
	if err != nil && !errors.Is(err, ErrHandoverChain) {
		t.Fatal("marker failure produced an unexpected error")
	}
}

func TestHandoverHolderRevisionWinsOverIncompleteHints(t *testing.T) {
	for _, stage := range []string{"before-both-markers", "between-markers", "stale-marker", "no-handover-from", "equal-top", "inconsistent-from"} {
		t.Run(stage, func(t *testing.T) {
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			a := savedHandover(t, s, 711)
			b := savedHandover(t, s, 712)
			b.State.Registration.Revision = 3
			if err = s.SaveSession(b.Key, b.State); err != nil {
				t.Fatal(err)
			}
			if err = s.MarkHandedOver(a.Key, b.Key); err != nil {
				t.Fatal(err)
			}
			a.State.Registration.Revision = 5
			a.State.Registration.HandoverFrom = &protocol.HandoverFrom{Revision: 3}
			if stage == "equal-top" {
				a.State.Registration.Revision = 3
				a.State.Registration.HandoverFrom = nil
			}
			if stage == "no-handover-from" {
				a.State.Registration.HandoverFrom = nil
			}
			if stage == "inconsistent-from" {
				a.State.Registration.HandoverFrom.Revision = 5
			}
			if err = s.SaveSession(a.Key, a.State); err != nil {
				t.Fatal(err)
			}
			if stage == "between-markers" {
				if err = s.ClearHandedOver(a.Key); err != nil {
					t.Fatal(err)
				}
			}
			reopened, err := Open(s.Dir())
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{a.Key, b.Key} {
				holder, err := reopened.HandoverHolder(key, a.State.Scope, a.State.Harness, a.State.ClientID, false)
				if stage == "equal-top" || stage == "inconsistent-from" {
					if !errors.Is(err, ErrHandoverNotFound) {
						t.Fatal("ambiguous or inconsistent revision did not fail closed")
					}
					continue
				}
				if err != nil || holder.Key != a.Key {
					t.Fatal("committed higher revision lost to an incomplete marker hint")
				}
			}
		})
	}
}
func TestHandoverHolderNeverSelectsDifferentSecret(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := savedHandover(t, s, 721)
	b := savedHandover(t, s, 722)
	b.State.Registration.Secret = "different-synthetic-proof"
	if err = s.SaveSession(b.Key, b.State); err != nil {
		t.Fatal(err)
	}
	holder, err := s.HandoverHolder(a.Key, a.State.Scope, a.State.Harness, a.State.ClientID, false)
	if err != nil || holder.Key != a.Key {
		t.Fatal("different secret overrode owned holder")
	}
}

func TestHandoverHolderRejectsMalformedKey(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"../outside", "", "not-a-session-key"} {
		if _, err = s.HandoverHolder(key, "scope", "claude", "client", false); !errors.Is(err, ErrHandoverNotFound) {
			t.Fatal("malformed key did not fail closed")
		}
	}
}
