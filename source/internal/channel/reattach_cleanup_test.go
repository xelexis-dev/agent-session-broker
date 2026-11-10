package channel

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// Start with genuine completed registration evidence, then model F1 removing R/G.
func completedCleanupState(t *testing.T, full ...bool) (string, *switchableUpstream, string) {
	t.Helper()
	dir := t.TempDir()
	shared := newSwitchableUpstream(t)
	first := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) {
		if len(full) > 0 && full[0] {
			writeDirectGrant(w, access.LegacyV1Scopes())
		} else {
			recoveryGrant(w, 1)
		}
	})
	waitFor(t, "completed registration", func() bool { return first.status("")["registered"] == true })
	first.stop()
	store, key, state := fixtureState(t, dir)
	old := state.Registration.RegistrationID
	if state.Registration.InitialAttempt != initialComplete {
		t.Fatal("fixture did not complete initial registration")
	}
	if err := shared.service.StopSession(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	if err := shared.service.RemoveDetachedSession(t.Context(), old, nil); err != nil {
		t.Fatal(err)
	}
	state.Credential = nil
	state.Device = &clientstate.DeviceAuthorization{Status: clientstate.DeviceRejected}
	if err := store.SaveSession(key, state); err != nil {
		t.Fatal(err)
	}
	return dir, shared, old
}
func missingCleanupRegistration(w http.ResponseWriter) {
	writeJSON(w, 409, map[string]string{"error": "identity_conflict", "handover_reason": "not-found"})
}

func TestReattachCleanupLostReplyReplaysThroughExplicitAttach(t *testing.T) {
	dir, shared, old := completedCleanupState(t)
	var allow atomic.Bool
	var fresh, proof atomic.Int32
	var mu sync.Mutex
	var requestID string
	f := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) {
		if r.PostFormValue("registration_id") != "" {
			proof.Add(1)
			missingCleanupRegistration(w)
			return
		}
		fresh.Add(1)
		_, _, saved := fixtureState(t, dir)
		if saved.Registration.RegistrationID != old || !saved.Direct.Reattach || saved.Direct.RequestID != r.PostFormValue("request_id") || r.PostFormValue("requested_nickname") != "direct-worker" {
			t.Error("fresh request not durably fenced")
		}
		mu.Lock()
		if requestID == "" {
			requestID = r.PostFormValue("request_id")
		} else if requestID != r.PostFormValue("request_id") {
			t.Error("explicit retry replaced N2")
		}
		mu.Unlock()
		if !allow.Load() {
			panic(http.ErrAbortHandler)
		}
		recoveryGrant(w, 2)
	})
	waitFor(t, "automatic not-found stopped", func() bool {
		m, _ := f.status("")["message"].(string)
		return strings.Contains(m, "registration-not-found")
	})
	if fresh.Load() != 0 {
		t.Fatal("automatic path started reattach")
	}
	if !f.call("", "attach_session", map[string]any{}).IsError {
		t.Fatal("lost reply accepted")
	}
	_, _, pending := fixtureState(t, dir)
	if pending.Direct == nil || !pending.Direct.Reattach || pending.Registration.RegistrationID != old || pending.Credential != nil {
		t.Fatal("lost reply discarded proof")
	}
	allow.Store(true)
	before := proof.Load()
	if f.call("", "attach_session", map[string]any{}).IsError {
		t.Fatal("explicit replay failed")
	}
	_, _, saved := fixtureState(t, dir)
	if proof.Load() != before || saved.Direct.RequestID != pending.Direct.RequestID || !saved.Direct.CreatedAt.Equal(pending.Direct.CreatedAt) || saved.Direct.Reattach || saved.Registration.RegistrationID == old || saved.Credential == nil {
		t.Fatal("replay did not preserve N2 or complete fresh registration")
	}
}

func TestReattachCleanupCredentialSavePreparesRestartRegistration(t *testing.T) {
	dir, shared, old := completedCleanupState(t)
	var savedSnapshot atomic.Bool
	snapshot := make(chan clientstate.SessionState, 1)
	second := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) {
		if r.PostFormValue("registration_id") != "" {
			missingCleanupRegistration(w)
			return
		}
		recoveryGrant(w, 2)
	}, func(f *deviceModeFixture) {
		f.persist = func(s *clientstate.Store, key string, state clientstate.SessionState) error {
			if state.Credential != nil && state.Registration.RegistrationID == "" && !savedSnapshot.Load() {
				if state.Registration.InitialAttempt != initialPending || state.Registration.PendingRequestID != "" || state.Registration.PendingLease != "" || state.Registration.PendingRevision != 0 || state.Registration.Detached || state.Direct.Reattach {
					t.Error("fresh credential save kept stale registration request")
				}
				if err := s.SaveSession(key, state); err != nil {
					return err
				}
				savedSnapshot.Store(true)
				snapshot <- state
				f.cancel()
				return nil
			}
			return s.SaveSession(key, state)
		}
	})
	waitFor(t, "automatic stopped", func() bool {
		m, _ := second.status("")["message"].(string)
		return strings.Contains(m, "registration-not-found")
	})
	_ = second.call("", "attach_session", map[string]any{})
	second.stop()
	if !savedSnapshot.Load() {
		t.Fatal("new credential snapshot not saved")
	}
	store, key, _ := fixtureState(t, dir)
	// Abrupt exit keeps the atomic commit; graceful cancellation may save a stop.
	// Restore the exact captured commit, without constructing registration fields.
	var committed clientstate.SessionState
	select {
	case committed = <-snapshot:
	case <-time.After(time.Second):
		t.Fatal("missing crash snapshot")
	}
	if err := store.SaveSession(key, committed); err != nil {
		t.Fatal(err)
	}
	_, _, saved := fixtureState(t, dir)
	if saved.Registration.InitialAttempt != initialPending || saved.Registration.RegistrationID != "" || saved.Credential == nil {
		t.Fatalf("restart snapshot initial=%s registrationPresent=%v credentialPresent=%v registerCalls=%d", saved.Registration.InitialAttempt, saved.Registration.RegistrationID != "", saved.Credential != nil, second.log.count("register_session"))
	}
	third := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) {
		t.Error("saved credential caused extra admission")
		recoveryGrant(w, 3)
	})
	waitFor(t, "automatic initial registration after restart", func() bool { return third.status("")["registered"] == true })
	if third.status("")["registrationId"] == old || third.log.count("register_session") != 1 {
		t.Fatal("restart did not create exactly one registration")
	}
}

func TestReattachCleanupSaveFailuresAndPermanentRefusalKeepProof(t *testing.T) {
	for _, mode := range []string{"pending-save", "credential-save", "forbidden", "insufficient-scope"} {
		t.Run(mode, func(t *testing.T) {
			dir, shared, old := completedCleanupState(t)
			var fresh atomic.Int32
			f := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) {
				if r.PostFormValue("registration_id") != "" {
					missingCleanupRegistration(w)
					return
				}
				fresh.Add(1)
				if mode == "insufficient-scope" {
					writeJSON(w, 403, map[string]string{"error": "insufficient_scope"})
					return
				}
				if mode == "forbidden" {
					writeJSON(w, 403, map[string]string{"error": "direct_registration_unavailable"})
					return
				}
				recoveryGrant(w, 2)
			}, func(f *deviceModeFixture) {
				f.persist = func(s *clientstate.Store, key string, state clientstate.SessionState) error {
					if mode == "pending-save" && state.Direct != nil && state.Direct.Reattach {
						return errors.New("synthetic pending save failure")
					}
					if mode == "credential-save" && state.Credential != nil {
						return errors.New("synthetic credential save failure")
					}
					return s.SaveSession(key, state)
				}
			})
			waitFor(t, "automatic stopped", func() bool {
				m, _ := f.status("")["message"].(string)
				return strings.Contains(m, "registration-not-found")
			})
			result := f.call("", "attach_session", map[string]any{})
			if !result.IsError {
				t.Fatal("fault accepted")
			}
			_, _, saved := fixtureState(t, dir)
			if saved.Registration.RegistrationID != old || saved.Credential != nil {
				t.Fatal("failure discarded old proof")
			}
			if mode == "pending-save" && fresh.Load() != 0 {
				t.Fatal("request escaped failed pending save")
			}
			if (mode == "forbidden" || mode == "insufficient-scope") && (saved.Direct.Reattach || saved.Direct.Status != "failed" || f.codeRequests() != 0) {
				t.Fatal("permanent refusal continued authorization")
			}
			if mode == "insufficient-scope" {
				m, _ := f.status("")["message"].(string)
				if !strings.Contains(m, "parent token cannot re-admit") {
					t.Fatal("missing fixed reattach refusal guidance")
				}
			}
		})
	}
}

func TestReattachCleanupCapabilityRequiresParent(t *testing.T) {
	for _, parent := range []bool{false, true} {
		o := durableOptions{}
		if parent {
			o.device = &deviceAuthOptions{authorizer: newDeviceAuthClient("https://broker/mcp?project=fixture", "synthetic-parent", nil)}
		}
		got := o.deliveryCapabilities(context.Background(), unavailableUpstream{}, "codex")
		if slices.Contains(got, broker.ReattachAfterCleanupCapability) != parent {
			t.Fatal("capability did not describe current parent authorizer")
		}
	}
}

func TestReattachCleanupPendingRequestExpiryAndProofReplay(t *testing.T) {
	for _, expired := range []bool{false, true} {
		store, err := clientstate.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		a := &sessionAuth{key: "synthetic-cleanup", explicitAttaching: 1, state: clientstate.SessionState{Registration: clientstate.Registration{RegistrationID: "saved-proof", Secret: "synthetic-resume-secret", Name: "saved-worker"}, Direct: &clientstate.DirectAuthorization{RequestID: "N2", CreatedAt: now, Status: "requested", RequestedNickname: "saved-worker", Reattach: true}}}
		if expired {
			a.state.Direct.CreatedAt = now.Add(-11 * time.Minute)
		}
		o := durableOptions{state: store, device: &deviceAuthOptions{clock: devicePollClock{now: func() time.Time { return now }}}}
		err = o.resetExplicitAdmission(a)
		if expired {
			if !errors.Is(err, errDirectPendingExpired) || a.state.Direct != nil || a.state.Registration.RegistrationID != "saved-proof" {
				t.Fatal("expired reattach did not preserve old proof")
			}
		} else if err != nil || a.state.Direct.RequestID != "N2" || a.state.Direct.RequestedNickname != "saved-worker" || !a.state.Direct.CreatedAt.Equal(now) {
			t.Fatal("unexpired explicit replay changed")
		}
	}
}

func TestReattachCleanupN1InsufficientScopeKeepsManualFallback(t *testing.T) {
	dir, shared, old := completedCleanupState(t, true)
	f := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 403, map[string]string{"error": "insufficient_scope"})
	})
	waitFor(t, "automatic insufficient scope stops", func() bool {
		m, _ := f.status("")["message"].(string)
		return strings.Contains(m, "insufficient-scope")
	})
	_ = f.call("", "attach_session", map[string]any{})
	if f.codeRequests() != 1 {
		t.Fatal("N1 existing manual approval fallback changed")
	}
	_, _, state := fixtureState(t, dir)
	if state.Registration.RegistrationID != old {
		t.Fatal("N1 manual fallback discarded proof")
	}
}

func TestReattachCleanupPendingRequestExcludesAutomaticRecovery(t *testing.T) {
	a := &sessionAuth{state: clientstate.SessionState{LastAdmissionMode: "auto", Registration: clientstate.Registration{RegistrationID: "saved", Secret: "synthetic-secret", InitialAttempt: initialComplete}, Direct: &clientstate.DirectAuthorization{RequestID: "N2", Status: "requested", CreatedAt: time.Now(), Reattach: true}}}
	b := &ingressBinding{mu: &sync.Mutex{}, durable: &durableRegistration{registrationID: "saved", secret: "synthetic-secret", initialAttempt: initialComplete, session: a}}
	o := durableOptions{device: &deviceAuthOptions{}}
	if a.recoveryEligibleLocked(o) {
		t.Fatal("pending explicit N2 still enrolls automatic recovery")
	}
	if o.automaticRegistrationAllowed(b) {
		t.Fatal("pending explicit N2 still starts automatic registration")
	}
}
