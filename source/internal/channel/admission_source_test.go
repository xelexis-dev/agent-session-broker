package channel

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
)

func TestChangedAdmissionTokenDiscardsRejectedDeviceRequest(t *testing.T) {
	store, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	auth := newDeviceAuthServer(t)
	oldClient := newDeviceAuthClient(auth.server.URL+"/mcp?project=brk", "asb_p_old-synthetic", auth.server.Client())
	opts := durableOptions{state: store, scope: "scope", project: "brk", device: &deviceAuthOptions{authorizer: oldClient}}
	id := singleIdentity()["11111111-1111-4111-8111-111111111111"]
	d, unlock, err := opts.loadSessionRegistration(id)
	if err != nil {
		t.Fatal(err)
	}
	// This D6 fixture represents an already registered conversation (A).
	d.session.state.Registration.RegistrationID = "synthetic-registration"
	if err = opts.transitionAdmission(d.session); err != nil {
		t.Fatal(err)
	}
	if err = opts.requestDeviceCode(context.Background(), d.session, id); err != nil {
		t.Fatal(err)
	}
	oldRequest := d.session.state.Device.RequestID
	d.session.state.Device.Status = clientstate.DeviceRejected
	if err = opts.persistSession(d.session); err != nil {
		t.Fatal(err)
	}
	unlock()
	opts.device.authorizer = newDeviceAuthClient(auth.server.URL+"/mcp?project=brk", "asb_p_new-synthetic", auth.server.Client())
	d, unlock, err = opts.loadSessionRegistration(id)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if d.session.state.Device != nil || d.session.state.Direct == nil || d.session.state.Direct.RequestID == oldRequest {
		t.Fatal("new token retained the previous token's rejected device request")
	}
}

func TestAdmissionSourceTransitionSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		credential, pending, legacy bool
	}{{"usable credential", true, false, false}, {"known pending", false, true, false}, {"legacy pending", false, true, true}, {"legacy rejected", false, false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			auth := newDeviceAuthServer(t)
			client := newDeviceAuthClient(auth.server.URL+"/mcp?project=brk", "asb_p_new-synthetic", auth.server.Client())
			store, _ := clientstate.Open(t.TempDir())
			state := clientstate.SessionState{AdmissionFingerprint: "old-synthetic-fingerprint", Registration: clientstate.Registration{Secret: "synthetic-secret", RegistrationID: "synthetic-registration", Name: "saved-worker"}, Device: &clientstate.DeviceAuthorization{Status: clientstate.DeviceRejected, RequestID: "previous-request"}, Direct: &clientstate.DirectAuthorization{Status: "redeemed", Restricted: true}}
			if tc.legacy {
				state.AdmissionFingerprint = ""
			}
			if tc.pending {
				state.Device.Status = clientstate.DevicePending
				state.Device.ExpiresAt = time.Now().Add(time.Hour)
			}
			if tc.credential {
				state.Credential = &clientstate.SessionCredential{AccessToken: "asb_s_synthetic-old", AdmissionMode: "auto"}
			}
			a := &sessionAuth{key: "synthetic-key", state: state}
			o := durableOptions{state: store, device: &deviceAuthOptions{authorizer: client}}
			if err := o.transitionAdmission(a); err != nil {
				t.Fatal(err)
			}
			preserve := tc.credential || (tc.legacy && tc.pending)
			if preserve {
				if a.state.AdmissionFingerprint != state.AdmissionFingerprint || a.state.Device.RequestID != "previous-request" {
					t.Fatal("usable or legacy pending state invalidated")
				}
				return
			}
			if a.state.AdmissionFingerprint != client.admissionFingerprint() || a.state.Device != nil || a.state.Direct.RequestID == "previous-request" || !a.state.Direct.Restricted {
				t.Fatal("transition source, request or scope protection")
			}
			id := a.state.Direct.RequestID
			before := a.state.AutoRecovery.Kind
			if err := o.transitionAdmission(a); err != nil {
				t.Fatal(err)
			}
			if a.state.Direct.RequestID != id || a.state.AutoRecovery.Kind != before {
				t.Fatal("same token reset transition")
			}
			if tc.legacy && before != "legacy-migration" {
				t.Fatal("legacy inferred token change")
			}
			if !tc.legacy && before != "token-changed" {
				t.Fatal("known change not distinguished")
			}
			report, _ := json.Marshal(a.report(false, "", ""))
			if strings.Contains(string(report), a.state.AdmissionFingerprint) {
				t.Fatal("full source fingerprint leaked")
			}
		})
	}
}

func TestAdmissionTransitionSaveFailureDoesNotPublishOrRequest(t *testing.T) {
	auth := newDeviceAuthServer(t)
	client := auth.client()
	store, _ := clientstate.Open(t.TempDir())
	a := &sessionAuth{key: "synthetic-key", state: clientstate.SessionState{AdmissionFingerprint: "old", Registration: clientstate.Registration{RegistrationID: "synthetic-registration", Secret: "synthetic-secret"}, Device: &clientstate.DeviceAuthorization{Status: clientstate.DeviceRejected, RequestID: "old-id"}}}
	o := durableOptions{state: store, device: &deviceAuthOptions{authorizer: client, persist: func(*clientstate.Store, string, clientstate.SessionState) error {
		return errors.New("synthetic disk error")
	}}}
	if o.transitionAdmission(a) == nil || a.state.AdmissionFingerprint != "old" || a.state.Device.RequestID != "old-id" {
		t.Fatal("failed save published token change")
	}
	if len(auth.requests) != 0 {
		t.Fatal("save failure issued admission request")
	}
}

func fixtureState(t *testing.T, dir string) (*clientstate.Store, string, clientstate.SessionState) {
	t.Helper()
	store, e := clientstate.Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	cid, _ := store.ClientID()
	key := clientstate.SessionKey("https://broker/mcp?project=brk", "claude", "11111111-1111-4111-8111-111111111111", cid)
	state, found, e := store.LoadSession(key)
	if e != nil || !found {
		t.Fatal("missing synthetic state")
	}
	return store, key, state
}
func newTokenFixture(f *deviceModeFixture) {
	f.configureDurable = func(o *durableOptions) { o.device.authorizer.(*deviceAuthClient).projectToken = "asb_p_new-synthetic" }
}
func TestKnownTokenChangeReauthorizesRejectedAndPreservesValidCredential(t *testing.T) {
	for _, usable := range []bool{false, true} {
		t.Run(fmt.Sprint("usable=", usable), func(t *testing.T) {
			dir := t.TempDir()
			shared := newSwitchableUpstream(t)
			var n atomic.Int32
			first := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) { recoveryGrant(w, n.Add(1)) })
			waitFor(t, "initial", func() bool { return first.status("")["registered"] == true })
			original := first.status("")["registrationId"]
			first.stop()
			store, key, state := fixtureState(t, dir)
			source := state.AdmissionFingerprint
			if !usable {
				state.Credential = nil
				state.Device = &clientstate.DeviceAuthorization{Status: clientstate.DeviceRejected, RequestID: "old-device-id"}
				state.Direct = &clientstate.DirectAuthorization{Status: "requested", RequestID: "old-direct-id", CreatedAt: time.Now()}
				if e := store.SaveSession(key, state); e != nil {
					t.Fatal(e)
				}
			}
			second := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) {
				if r.PostFormValue("request_id") == "old-direct-id" || r.PostFormValue("requested_nickname") != "" {
					t.Error("token change replayed old request/name")
				}
				recoveryGrant(w, n.Add(1))
			}, newTokenFixture)
			waitFor(t, "second registration", func() bool { return second.status("")["registered"] == true })
			if usable {
				_, _, saved := fixtureState(t, dir)
				if saved.AdmissionFingerprint != source || n.Load() != 1 {
					t.Fatal("valid credential discarded or relabeled")
				}
				rejectFirst(second)
				waitFor(t, "later401 admission", func() bool { return n.Load() == 2 })
				waitFor(t, "later401 resumed", func() bool { s := second.status(""); return s["registered"] == true && s["registrationId"] == original })
			}
			_, _, saved := fixtureState(t, dir)
			if saved.AdmissionFingerprint == source || second.status("")["registrationId"] != original || second.codeRequests() != 0 || second.log.count("register_session") != 0 {
				t.Fatal("changed token did not resume same registration")
			}
		})
	}
}

func TestChangedTokenChoosesManualPolicyWithoutReplayingOldPending(t *testing.T) {
	dir := t.TempDir()
	shared := newSwitchableUpstream(t)
	first := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) { recoveryGrant(w, 1) })
	waitFor(t, "initial", func() bool { return first.status("")["registered"] == true })
	original := first.status("")["registrationId"]
	first.stop()
	store, key, state := fixtureState(t, dir)
	state.Credential = nil
	state.Device = &clientstate.DeviceAuthorization{Status: clientstate.DevicePending, RequestID: "old-code-request", DeviceCode: "old-device-code", ExpiresAt: time.Now().Add(time.Hour)}
	state.Direct = &clientstate.DirectAuthorization{Status: "requested", RequestID: "old-direct-request", CreatedAt: time.Now(), Restricted: true}
	if e := store.SaveSession(key, state); e != nil {
		t.Fatal(e)
	}
	second := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) {
		if r.PostFormValue("request_id") == "old-direct-request" {
			t.Error("replayed previous direct request")
		}
		writeJSON(w, 403, map[string]string{"error": "insufficient_scope"})
	}, newTokenFixture)
	waitFor(t, "new manual code", func() bool { return second.codeRequests() == 1 })
	second.auth.mu.Lock()
	for _, r := range second.auth.requests {
		if r.Form.Get("device_code") == "old-device-code" {
			t.Error("polled old device code")
		}
	}
	second.auth.mu.Unlock()
	second.grant("11111111-1111-4111-8111-111111111111", fixtureToken("asb_s_"), "direct-worker")
	waitFor(t, "manual grant resumes existing row", func() bool { s := second.status(""); return s["registered"] == true && s["registrationId"] == original })
	if second.log.count("register_session") != 0 {
		t.Fatal("manual token change created a registration")
	}
}

func TestLegacyMigrationBudgetAndManualNoDeviceFallback(t *testing.T) {
	for _, auto := range []bool{false, true} {
		t.Run(fmt.Sprint("auto=", auto), func(t *testing.T) {
			dir := t.TempDir()
			shared := newSwitchableUpstream(t)
			var n atomic.Int32
			first := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) { recoveryGrant(w, n.Add(1)) })
			waitFor(t, "initial", func() bool { return first.status("")["registered"] == true })
			first.stop()
			store, key, state := fixtureState(t, dir)
			state.Credential = nil
			state.AdmissionFingerprint = ""
			state.LastAdmissionMode = "manual"
			if auto {
				state.LastAdmissionMode = "auto"
			}
			state.Device = &clientstate.DeviceAuthorization{Status: clientstate.DeviceRejected}
			if e := store.SaveSession(key, state); e != nil {
				t.Fatal(e)
			}
			handler := func(w http.ResponseWriter, r *http.Request) {
				n.Add(1)
				writeJSON(w, 403, map[string]string{"error": "insufficient_scope"})
			}
			second := recoveryFixture(t, dir, shared, handler)
			waitFor(t, "legacy direct stopped", func() bool {
				_, _, s := fixtureState(t, dir)
				return s.AutoRecovery != nil && s.AutoRecovery.StopReason != ""
			})
			if second.codeRequests() != 0 || n.Load() != 2 {
				t.Fatal("legacy requested device or repeated migration")
			}
			second.stop()
			third := recoveryFixture(t, dir, shared, handler)
			_ = third.status("")
			_ = third.call("", "list_sessions", map[string]any{})
			if n.Load() != 2 || third.codeRequests() != 0 {
				t.Fatal("restart reset migration budget")
			}
		})
	}
}

func TestNetworkFailureKeepsCredentialAndSource(t *testing.T) {
	f := recoveryFixture(t, t.TempDir(), nil, func(w http.ResponseWriter, r *http.Request) { recoveryGrant(w, 1) })
	waitFor(t, "initial", func() bool { return f.status("")["registered"] == true })
	_, _, before := fixtureState(t, f.stateDir)
	f.broker.offline.Store(true)
	waitFor(t, "network resume attempt", func() bool { return f.log.count("resume_session") > 0 })
	_, _, after := fixtureState(t, f.stateDir)
	if after.Credential == nil || after.Credential.AccessToken != before.Credential.AccessToken || after.AdmissionFingerprint != before.AdmissionFingerprint || after.AutoRecovery != nil {
		t.Fatal("network failure was treated as credential rejection")
	}
}

func TestChangedTokenDetachedResumeNeverRegisters(t *testing.T) {
	dir := t.TempDir()
	shared := newSwitchableUpstream(t)
	first := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) { recoveryGrant(w, 1) })
	waitFor(t, "initial", func() bool { return first.status("")["registered"] == true })
	first.stop()
	store, key, state := fixtureState(t, dir)
	state.Credential = nil
	state.Device = &clientstate.DeviceAuthorization{Status: clientstate.DeviceRejected}
	if e := store.SaveSession(key, state); e != nil {
		t.Fatal(e)
	}
	second := recoveryFixture(t, dir, shared, func(w http.ResponseWriter, r *http.Request) { recoveryGrant(w, 2) }, func(f *deviceModeFixture) {
		newTokenFixture(f)
		f.wrapUpstream = func(_ string, u Upstream) Upstream { return &recoveryUpstream{Upstream: u, resumeCode: "detached"} }
	})
	waitFor(t, "detached stop", func() bool {
		m, _ := second.status("")["message"].(string)
		return strings.Contains(m, "registration-detached")
	})
	if second.log.count("register_session") != 0 || second.status("")["registered"] == true || second.codeRequests() != 0 {
		t.Fatal("token change recreated detached registration")
	}
}

// fixtureToken returns a random credential of the real minted shape, so no
// token-shaped literal ships in the public source.
func fixtureToken(prefix string) string { return prefix + rand.Text() + rand.Text() }
