package channel

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
)

func TestKnownDetachedStopsAutomaticReadmissionEvenAfterCredentialRejection(t *testing.T) {
	store, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := clientstate.SessionKey("scope", "codex", "11111111-1111-4111-8111-111111111111", "test-client")
	a := &sessionAuth{key: key, state: clientstate.SessionState{LastAdmissionMode: "auto", Registration: clientstate.Registration{RegistrationID: "saved", Secret: "synthetic-secret"}, Credential: &clientstate.SessionCredential{AccessToken: "asb_s_synthetic", AdmissionMode: "auto"}}}
	a.deriveStatus(time.Now())
	b := &ingressBinding{mu: &sync.Mutex{}, durable: &durableRegistration{key: key, registrationID: "saved", secret: "synthetic-secret", session: a}}
	opts := durableOptions{state: store, device: &deviceAuthOptions{}}
	opts.detachedObserved(b)
	opts.rejectCredentialIfCurrent(a, "asb_s_synthetic")
	a.mu.Lock()
	eligible := a.recoveryEligibleLocked(opts)
	a.mu.Unlock()
	if eligible || opts.automaticRegistrationAllowed(b) {
		t.Fatal("known detached registration still authorizes automatic direct recovery after credential rejection")
	}
}

func TestAuthorizedProgressRequiresAnActualAttempt(t *testing.T) {
	for _, pending := range []bool{false, true} {
		a := &sessionAuth{state: clientstate.SessionState{Registration: clientstate.Registration{Secret: "synthetic", InitialAttempt: initialComplete, RegistrationID: "saved"}, Credential: &clientstate.SessionCredential{AccessToken: "asb_s_synthetic"}}}
		if pending {
			a.state.Registration.RegistrationID = ""
			a.state.Registration.InitialAttempt = initialPending
		}
		a.deriveStatus(time.Now())
		out := a.report(false, "", "")
		if strings.Contains(strings.ToLower(out.Message), "in progress") != pending {
			t.Fatalf("pending=%v message=%s", pending, out.Message)
		}
	}
}

func TestDetachedStatusOnlyClaimsProgressDuringExplicitRevival(t *testing.T) {
	a := &sessionAuth{status: authAuthorized, state: clientstate.SessionState{Registration: clientstate.Registration{Detached: true, Secret: "synthetic", RegistrationID: "saved"}}}
	for _, explicit := range []bool{false, true} {
		a.registrationAttempts = 1
		a.explicitAttaching = 0
		if explicit {
			a.explicitAttaching = 1
		}
		out := a.report(false, "", "saved")
		if strings.Contains(out.Message, "Registration is in progress.") != explicit {
			t.Fatalf("explicit=%v message=%s", explicit, out.Message)
		}
		if strings.Contains(out.Message, detachedGuidance) == explicit {
			t.Fatalf("explicit=%v detached guidance=%s", explicit, out.Message)
		}
	}
}
