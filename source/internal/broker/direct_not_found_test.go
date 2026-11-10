package broker

import (
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"testing"
)

func TestDirectMissingRegistrationKeepsExistingProofConflicts(t *testing.T) {
	s, _ := clockedService(t, t.TempDir())
	a := durableRegister(1, "direct-missing", "client-missing-001", durableSecretA, "lease-initial", "request-initial")
	first, err := s.RegisterSession(t.Context(), a)
	if err != nil || first.Code != "" {
		t.Fatal("fixture registration")
	}
	for _, mutation := range []string{"valid", "harness", "session", "client", "secret", "missing"} {
		t.Run(mutation, func(t *testing.T) {
			harness, session, client, id, secret := a.Harness, a.SessionID, a.ClientID, first.Session.RegistrationID, a.Secret
			switch mutation {
			case "harness":
				harness = "codex"
			case "session":
				session = "11111111-1111-4111-8111-999999999999"
			case "client":
				client = "different-client"
			case "missing":
				id = "AAAAAAAAAAAAAAAAAAAAAAAAAA"
			case "secret":
				secret = durableSecretB
			}
			proof, err := s.InspectDirectRegistration(t.Context(), harness, session, client, id, secret)
			if mutation == "valid" {
				if err != nil || proof.RegistrationID != id {
					t.Fatal("valid proof rejected")
				}
				return
			}
			var refusal protocol.HandoverRefusal
			missing := errors.As(err, &refusal) && refusal.Reason == protocol.CodeNotFound
			if err == nil || missing != (mutation == "missing") || proof.RegistrationID != "" || proof.OwnerID != "" || proof.Name != "" {
				t.Fatal("missing classification changed conflicts or disclosed record metadata")
			}
		})
	}
}
