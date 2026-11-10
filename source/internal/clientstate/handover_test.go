package clientstate

import (
	"errors"
	"fmt"
	"testing"
)

func savedHandover(t *testing.T, s *Store, n int) SavedConversation {
	t.Helper()
	state := SessionState{Scope: "synthetic-scope", Harness: "claude", SessionID: fmt.Sprintf("%08d-1111-4111-8111-111111111111", n), ClientID: "synthetic-client", Registration: Registration{Revision: uint64(n), Secret: "synthetic-proof", RegistrationID: "AAAAAAAAAAAAAAAAAAAAAAAAAA"}}
	key := SessionKey(state.Scope, state.Harness, state.SessionID, state.ClientID)
	if err := s.SaveSession(key, state); err != nil {
		t.Fatal(err)
	}
	return SavedConversation{Key: key, State: state}
}
func TestHandoverHolderBoundaries(t *testing.T) {
	for _, links := range []int{0, 1, 8, 9} {
		t.Run(fmt.Sprint(links), func(t *testing.T) {
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			start := savedHandover(t, s, 1)
			current := start
			for i := 0; i < links; i++ {
				next := savedHandover(t, s, i+2)
				if err := s.MarkHandedOver(current.Key, next.Key); err != nil {
					t.Fatal(err)
				}
				current = next
			}
			holder, err := s.HandoverHolder(start.Key, start.State.Scope, start.State.Harness, start.State.ClientID, false)
			if links > 8 {
				if !errors.Is(err, ErrHandoverChain) {
					t.Fatal("overlong chain accepted")
				}
			} else if err != nil || holder.Key != current.Key {
				t.Fatal("bounded holder lookup failed")
			}
		})
	}
}
func TestHandoverHolderRejectsCyclesAndForeignState(t *testing.T) {
	for _, kind := range []string{"cycle", "scope", "harness", "client", "missing", "source-detached", "holder-detached"} {
		t.Run(kind, func(t *testing.T) {
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			a := savedHandover(t, s, 10)
			b := savedHandover(t, s, 11)
			if err := s.MarkHandedOver(a.Key, b.Key); err != nil {
				t.Fatal(err)
			}
			want := ErrHandoverChain
			switch kind {
			case "cycle":
				if err := s.MarkHandedOver(b.Key, a.Key); err != nil {
					t.Fatal(err)
				}
			case "scope":
				b.State.Scope = "foreign"
			case "harness":
				b.State.Harness = "codex"
			case "client":
				b.State.ClientID = "foreign-client"
			case "missing":
				if err := s.MarkHandedOver(a.Key, SessionKey("s", "h", "id", "c")); err != nil {
					t.Fatal(err)
				}
			case "source-detached":
				a.State.Registration.Detached = true
				if err := s.SaveSession(a.Key, a.State); err != nil {
					t.Fatal(err)
				}
				want = ErrHandoverDetached
			case "holder-detached":
				b.State.Registration.Detached = true
				want = ErrHandoverDetached
			}
			if err := s.SaveSession(b.Key, b.State); err != nil {
				t.Fatal(err)
			}
			if _, err := s.HandoverHolder(a.Key, a.State.Scope, a.State.Harness, a.State.ClientID, false); !errors.Is(err, want) {
				t.Fatal("unsafe chain was followed")
			}
		})
	}
}
func TestHandoverMarkerClearSurvivesLateStateWrite(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := savedHandover(t, s, 20)
	b := savedHandover(t, s, 21)
	if err := s.MarkHandedOver(a.Key, b.Key); err != nil {
		t.Fatal(err)
	}
	stale, _, err := s.LoadSession(a.Key)
	if err != nil || stale.Registration.HandedOverTo != b.Key {
		t.Fatal("marker setup")
	}
	if err := s.ClearHandedOver(a.Key); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSession(a.Key, stale); err != nil {
		t.Fatal(err)
	}
	current, _, err := s.LoadSession(a.Key)
	if err != nil || current.Registration.HandedOverTo != "" {
		t.Fatal("stale state resurrected cleared marker")
	}
	if err := s.ClearHandedOver(a.Key); err != nil {
		t.Fatal("clear is not idempotent")
	}
}

func TestHandoverHolderExplicitDetached(t *testing.T) {
	for _, kind := range []string{"source", "holder", "unlinked-holder"} {
		t.Run(kind, func(t *testing.T) {
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			a := savedHandover(t, s, 40)
			b := savedHandover(t, s, 41)
			if kind != "unlinked-holder" {
				if err := s.MarkHandedOver(a.Key, b.Key); err != nil {
					t.Fatal(err)
				}
			}
			detached := b
			if kind == "source" {
				detached = a
				detached.State.Registration.HandedOverTo = b.Key
			}
			detached.State.Registration.Detached = true
			if err := s.SaveSession(detached.Key, detached.State); err != nil {
				t.Fatal(err)
			}
			if _, err := s.HandoverHolder(a.Key, a.State.Scope, a.State.Harness, a.State.ClientID, false); !errors.Is(err, ErrHandoverDetached) {
				t.Fatal("automatic lookup accepted detached registration")
			}
			holder, err := s.HandoverHolder(a.Key, a.State.Scope, a.State.Harness, a.State.ClientID, true)
			if err != nil || holder.Key != b.Key {
				t.Fatal("explicit lookup could not resolve detached holder")
			}
		})
	}
}
