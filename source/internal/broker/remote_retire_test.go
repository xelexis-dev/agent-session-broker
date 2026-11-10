package broker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func retireCheck(Session, string, []Session) (protocol.ManagementEvent, error) {
	return protocol.ManagementEvent{ActorName: "manager", ActorRegistrationID: "OTHER"}, nil
}
func retireCleanup(_ access.RetirementTarget, detach func() error) ([]protocol.RevokedCredential, error) {
	if detach != nil {
		if err := detach(); err != nil {
			return nil, err
		}
	}
	return []protocol.RevokedCredential{}, nil
}
func TestRetireRegistrationStatesAndStaleProof(t *testing.T) {
	for _, state := range []string{"live", "missing", "detached"} {
		t.Run(state, func(t *testing.T) {
			s, now := clockedService(t, "")
			defer s.Close()
			r := durableRegister(1, "X", "client-retire", durableSecretA, "retire-lease", "retire-request")
			registered, err := s.RegisterSession(t.Context(), r)
			if err != nil || registered.Code != "" {
				t.Fatal(err, registered.Code)
			}
			id := registered.Session.RegistrationID
			if state == "missing" {
				*now = now.Add(remoteLeaseDuration + time.Second)
				s.GetSession(t.Context(), id)
			}
			if state == "detached" {
				if err := s.StopSession(t.Context(), id); err != nil {
					t.Fatal(err)
				}
			}
			out, err := s.RetireManaged(t.Context(), protocol.RetireRequest{RegistrationID: id}, retireCheck, retireCleanup)
			if err != nil || !out.OK {
				t.Fatal(err)
			}
			if _, err := s.GetSession(t.Context(), id); !errors.Is(err, ErrSessionNotFound) {
				t.Fatal("registration retained")
			}
			// A proof inspected before retirement must never authorize a later exchange.
			called := false
			err = s.WithDirectRegistration(t.Context(), r, id, false, func(DirectProof) error { called = true; return nil })
			if err == nil || called {
				t.Fatal("stale registration proof reached exchange")
			}
		})
	}
}
func TestRetireStorageFailureAndRetry(t *testing.T) {
	for _, failure := range []string{"detach", "policy", "remove"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			s, err := NewSQLitePersistentRemoteService(root)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			r := durableRegister(1, "X", "client-retire", durableSecretA, "retire-lease", "retire-request")
			reg, err := s.RegisterSession(t.Context(), r)
			if err != nil || reg.Code != "" {
				t.Fatal(err)
			}
			if failure == "detach" {
				s.store.database.Close()
			}
			out, err := s.RetireManaged(t.Context(), protocol.RetireRequest{RegistrationID: reg.Session.RegistrationID}, retireCheck, func(target access.RetirementTarget, detach func() error) ([]protocol.RevokedCredential, error) {
				if err := detach(); err != nil {
					return nil, protocol.RejectRetire("storage")
				}

				if failure == "policy" {
					return nil, protocol.RejectRetire("storage")
				}
				if failure == "remove" {
					s.store.database.Close()
				}
				return []protocol.RevokedCredential{}, nil
			})
			var rejection protocol.RetireRejection
			if out.OK || !errors.As(err, &rejection) {
				t.Fatal("failure not reported", err)
			}
			want := "cleanup-required"
			if failure == "detach" {
				want = "storage"
			}
			if rejection.Code != want {
				t.Fatal(rejection)
			}
			current, err := s.GetSession(t.Context(), reg.Session.RegistrationID)
			if err != nil {
				t.Fatal("retry record lost", err)
			}
			if failure != "detach" && current.ErrorCode != CodeDetached {
				t.Fatal("target revived")
			}
			if failure == "remove" {
				s.Close()
				restored, err := NewSQLitePersistentRemoteService(root)
				if err != nil {
					t.Fatal("restart cleanup", err)
				}
				defer restored.Close()
				completed, err := restored.RetireManaged(t.Context(), protocol.RetireRequest{RegistrationID: reg.Session.RegistrationID}, retireCheck, retireCleanup)
				if err != nil || !completed.OK {
					t.Fatal("remove retry by ID failed", err)
				}
			}
			if failure == "policy" {
				out, err = s.RetireManaged(t.Context(), protocol.RetireRequest{RegistrationID: reg.Session.RegistrationID}, retireCheck, retireCleanup)
				if err != nil || !out.OK {
					t.Fatal("retry failed", err)
				}
			}
		})
	}
}
func TestDirectProofExchangeBlocksRetirement(t *testing.T) {
	s := NewRemoteService()
	defer s.Close()
	r := durableRegister(1, "X", "client-retire", durableSecretA, "retire-lease", "retire-request")
	reg, err := s.RegisterSession(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.WithDirectRegistration(context.Background(), r, reg.Session.RegistrationID, false, func(DirectProof) error { close(entered); <-release; return nil })
	}()
	<-entered
	retired := make(chan error, 1)
	go func() {
		_, err := s.RetireManaged(context.Background(), protocol.RetireRequest{RegistrationID: reg.Session.RegistrationID}, retireCheck, retireCleanup)
		retired <- err
	}()
	select {
	case <-retired:
		t.Fatal("retire bypassed exchange fence")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-retired; err != nil {
		t.Fatal(err)
	}
}
