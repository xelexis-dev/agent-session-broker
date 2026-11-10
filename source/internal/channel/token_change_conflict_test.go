package channel

import (
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExplicitAttachEscapesTokenChangeIdentityConflict(t *testing.T) {
	for _, manual := range []bool{false, true} {
		for _, takeover := range []bool{false, true} {
			t.Run(fmt.Sprintf("manual=%t/takeover=%t", manual, takeover), func(t *testing.T) {
				testExplicitAttachTokenChange(t, manual, takeover)
			})
		}
	}
}

func testExplicitAttachTokenChange(t *testing.T, manual, takeover bool) {
	dir := t.TempDir()
	shared := newSwitchableUpstream(t)
	first := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) { recoveryGrant(w, 1) })
	waitFor(t, "initial registration", func() bool { return first.status("")["registered"] == true })
	original := first.status("")["registrationId"]
	first.stop()
	store, key, state := fixtureState(t, dir)
	state.Credential = nil
	if manual {
		state.LastAdmissionMode = "manual"
	}
	state.Direct = &clientstate.DirectAuthorization{Status: "requested", RequestID: "old-token-request", CreatedAt: time.Now().Add(-11 * time.Minute)}
	if err := store.SaveSession(key, state); err != nil {
		t.Fatal(err)
	}
	var allowed atomic.Bool
	var attempts atomic.Int32
	second := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.PostFormValue("request_id") == "old-token-request" || r.PostFormValue("registration_id") != original || r.PostFormValue("resume_secret") == "" {
			t.Error("token transition lost saved proof or replayed old request")
		}
		if !allowed.Load() {
			writeJSON(w, 409, map[string]string{"error": "identity_conflict"})
			return
		}
		recoveryGrant(w, 2)
	}, newTokenFixture)
	waitFor(t, "terminal conflict", func() bool {
		m, _ := second.status("")["message"].(string)
		return strings.Contains(m, "identity-conflict")
	})
	allowed.Store(true)
	args := map[string]any{}
	if takeover {
		args["takeover"] = "direct-worker"
	}
	result := second.call("", "attach_session", args)
	if result.IsError || attempts.Load() != 2 {
		t.Fatal("explicit attach remained trapped in terminal recovery instead of retrying direct admission")
	}
	waitFor(t, "same registration recovered", func() bool { s := second.status(""); return s["registered"] == true && s["registrationId"] == original })
	sessions, err := shared.service.ListSessions(t.Context())
	if err != nil || len(sessions) != 1 || sessions[0].RegistrationID != original {
		t.Fatal("explicit recovery created a duplicate registration")
	}
}

func TestExplicitAttachReplacesExpiredDirectRequest(t *testing.T) {
	dir := t.TempDir()
	shared := newSwitchableUpstream(t)
	first := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) { recoveryGrant(w, 1) })
	waitFor(t, "initial registration", func() bool { return first.status("")["registered"] == true })
	first.stop()
	store, key, state := fixtureState(t, dir)
	state.Credential = nil
	state.Direct = &clientstate.DirectAuthorization{Status: "requested", RequestID: "expired-request", CreatedAt: time.Now().Add(-11 * time.Minute)}
	if err := store.SaveSession(key, state); err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	second := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.PostFormValue("request_id") == "expired-request" {
			t.Error("expired request replayed")
		}
		recoveryGrant(w, 2)
	})
	waitFor(t, "expired recovery stopped", func() bool {
		m, _ := second.status("")["message"].(string)
		return strings.Contains(m, "pending-expired")
	})
	if attempts.Load() != 0 {
		t.Fatal("expired automatic request reached server")
	}
	if second.call("", "attach_session", map[string]any{}).IsError {
		t.Fatal("explicit expired recovery failed")
	}
	waitFor(t, "registration recovered", func() bool { return second.status("")["registered"] == true })
	if attempts.Load() != 1 {
		t.Fatal("explicit expired recovery did not send exactly one fresh request")
	}
}

// Residual for the cross-owner design: explicit admission still presents its saved
// proof after panel removal; it must not silently discard that proof on a conflict.
// R3 intentionally changes administrator-detach recovery: explicit attach may
// obtain a fresh credential from its parent only after a not-found response.
func TestExplicitAttachAfterAdminDetachReadmitsWithParent(t *testing.T) {
	dir := t.TempDir()
	shared := newSwitchableUpstream(t)
	first := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) { recoveryGrant(w, 1) })
	waitFor(t, "initial registration", func() bool { return first.status("")["registered"] == true })
	original := first.status("")["registrationId"].(string)
	first.stop()
	store, key, state := fixtureState(t, dir)
	state.Credential = nil
	if err := store.SaveSession(key, state); err != nil {
		t.Fatal(err)
	}
	var detached atomic.Bool
	var attempts atomic.Int32
	second := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.PostFormValue("registration_id") == "" {
			if !detached.Load() || r.PostFormValue("requested_nickname") != "direct-worker" {
				t.Error("fresh request lacked explicit not-found or stored name")
			}
			recoveryGrant(w, 2)
			return
		}
		if r.PostFormValue("registration_id") != original {
			t.Error("wrong saved proof")
		}
		body := map[string]string{"error": "identity_conflict"}
		if detached.Load() {
			body["handover_reason"] = "not-found"
		}
		writeJSON(w, 409, body)
	}, newTokenFixture)
	waitFor(t, "ownership refusal", func() bool {
		m, _ := second.status("")["message"].(string)
		return strings.Contains(m, "identity-conflict")
	})
	if attempts.Load() != 1 {
		t.Fatal("identity conflict retried")
	}
	// Model the administrator removing the broker record as well as its grant.
	if err := shared.service.StopSession(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	if err := shared.service.RemoveDetachedSession(t.Context(), original, nil); err != nil {
		t.Fatal(err)
	}
	detached.Store(true)
	if second.call("", "attach_session", map[string]any{}).IsError {
		t.Fatal("explicit parent readmission failed")
	}
	waitFor(t, "new registration", func() bool { s := second.status(""); return s["registered"] == true && s["registrationId"] != original })
	if attempts.Load() != 3 || second.codeRequests() != 0 {
		t.Fatal("fresh authorization duplicated or manual approval requested")
	}
	_, _, saved := fixtureState(t, dir)
	if saved.Registration.RegistrationID == original || saved.Registration.Name != "direct-worker" {
		t.Fatal("fresh credential or registration not saved")
	}
}
