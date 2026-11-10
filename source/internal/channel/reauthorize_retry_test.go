package channel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type scriptedDirect struct {
	requests []directCodeRequest
	failures []error
	devices  int
}

func (s *scriptedDirect) requestCode(context.Context, deviceCodeRequest) (deviceAuthorization, error) {
	s.devices++
	return deviceAuthorization{}, errors.New("unexpected device request")
}
func (s *scriptedDirect) redeem(context.Context, string) (sessionGrant, tokenOutcome, error) {
	return sessionGrant{}, tokenInvalidGrant, nil
}
func (s *scriptedDirect) discover(context.Context) (bool, error) { return true, nil }
func (s *scriptedDirect) scoped(context.Context) (bool, error)   { return true, nil }
func (s *scriptedDirect) exchange(_ context.Context, r directCodeRequest) (directGrant, error) {
	s.requests = append(s.requests, r)
	i := len(s.requests) - 1
	if i < len(s.failures) && s.failures[i] != nil {
		return directGrant{}, s.failures[i]
	}
	return directGrant{sessionGrant: sessionGrant{AccessToken: "asb_s_synthetic-new", TokenType: "Bearer", ExpiresIn: 3600, SessionGrantID: "synthetic-grant", Nickname: "saved-worker"}, Mode: "auto", Scopes: []string{"session:register"}}, nil
}
func retryState(t *testing.T, auth *scriptedDirect, clock devicePollClock) (durableOptions, *sessionAuth, broker.AttachRequest) {
	t.Helper()
	store, _ := clientstate.Open(t.TempDir())
	a := &sessionAuth{key: "synthetic-key", state: clientstate.SessionState{LastAdmissionMode: "auto", Registration: clientstate.Registration{RegistrationID: "synthetic-id", Name: "saved-worker", Secret: "synthetic-secret"}, AutoRecovery: &clientstate.AutoRecovery{}}}
	o := durableOptions{state: store, device: &deviceAuthOptions{authorizer: auth, clock: clock}}
	id := singleIdentity()["11111111-1111-4111-8111-111111111111"]
	return o, a, id
}
func TestAutoRecoveryRetryBackoffSameRequestAndCancellation(t *testing.T) {
	now := time.Now()
	var delays []time.Duration
	auth := &scriptedDirect{failures: []error{&directTransientError{}, &directTransientError{}, &directTransientError{retryAfter: 5 * time.Minute}, errProjectTokenRejected}}
	o, a, id := retryState(t, auth, devicePollClock{now: func() time.Time { return now }, sleep: func(ctx context.Context, d time.Duration) error {
		delays = append(delays, d)
		now = now.Add(d)
		return ctx.Err()
	}})
	if o.reauthorize(t.Context(), a, id) {
		t.Fatal("permanent rejection recovered")
	}
	if !reflect.DeepEqual(delays, []time.Duration{30 * time.Second, 60 * time.Second, 5 * time.Minute}) {
		t.Fatalf("unexpected delays: %v", delays)
	}
	for _, r := range auth.requests {
		if r.RequestID != auth.requests[0].RequestID || r.Nickname != "" || r.RegistrationID != "synthetic-id" {
			t.Fatal("retry lost request/identity")
		}
	}
	before := len(auth.requests)
	if o.reauthorize(t.Context(), a, id) || len(auth.requests) != before {
		t.Fatal("terminal state retried")
	}
	for i, want := range []time.Duration{30, 60, 120, 240, 480, 600, 600} {
		if automaticRecoveryBackoff(i) != want*time.Second {
			t.Fatal("backoff not capped")
		}
	}
	auth = &scriptedDirect{failures: []error{&directTransientError{}}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	o, a, id = retryState(t, auth, devicePollClock{now: time.Now, sleep: func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }})
	if o.reauthorize(ctx, a, id) || len(auth.requests) != 1 {
		t.Fatal("cancellation did not stop retry")
	}
}
func TestAutoRecoveryExpiredPendingAndExpectedCredential(t *testing.T) {
	auth := &scriptedDirect{}
	o, a, id := retryState(t, auth, realPollClock())
	a.state.Direct = &clientstate.DirectAuthorization{Status: "requested", RequestID: "old-request", CreatedAt: time.Now().Add(-11 * time.Minute)}
	if o.reauthorize(t.Context(), a, id) || len(auth.requests) != 0 || a.state.AutoRecovery.StopReason != "pending-expired" {
		t.Fatal("expired direct request retried")
	}
	a.state.Credential = &clientstate.SessionCredential{AccessToken: "asb_s_synthetic-new", AdmissionMode: "auto"}
	o.rejectCredentialIfCurrent(a, "asb_s_synthetic-old")
	if a.state.Credential == nil {
		t.Fatal("retired receiver discarded new credential")
	}
}
func TestRecoveryDirectHTTPClassificationAndRetryAfter(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls int
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if status == 429 {
					w.Header().Set("Retry-After", "3600")
				}
				writeJSON(w, status, map[string]string{"error": "synthetic"})
			}))
			defer server.Close()
			client := newInstantDeviceAuthClient(server.URL+"/mcp?project=brk", "asb_p_synthetic", server.Client())
			_, err := client.exchange(t.Context(), directCodeRequest{Recovery: true})
			var temporary *directTransientError
			transient := errors.As(err, &temporary)
			if calls != 1 || transient != (status >= 500 || status == 429) {
				t.Fatal("wrong transient/permanent classification or early retry")
			}
			if status == 429 && temporary.retryAfter != time.Hour {
				t.Fatal("Retry-After truncated")
			}
		})
	}
}

func TestRestrictedRecoveryUsesValidatedDirectGrantWithoutDiscovery(t *testing.T) {
	var discovery, exchange int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/capabilities" {
			discovery++
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/oauth/session_registration" {
			exchange++
			writeDirectGrant(w, []string{"session:register"})
			return
		}
		t.Error("unexpected endpoint")
	}))
	defer server.Close()
	auth := newInstantDeviceAuthClient(server.URL+"/mcp?project=brk", "asb_p_synthetic", server.Client())
	store, _ := clientstate.Open(t.TempDir())
	a := &sessionAuth{key: "synthetic-key", state: clientstate.SessionState{RestrictedAdmission: true, LastAdmissionMode: "auto", Registration: clientstate.Registration{RegistrationID: "synthetic-id", Secret: "synthetic-secret"}, AutoRecovery: &clientstate.AutoRecovery{}}}
	o := durableOptions{state: store, device: &deviceAuthOptions{authorizer: auth, clock: realPollClock()}}
	if !o.reauthorize(t.Context(), a, broker.AttachRequest{Harness: "codex", SessionID: "synthetic-session"}) || discovery != 0 || exchange != 1 {
		t.Fatal("discovery outage stopped validated direct recovery")
	}
}

func TestRecoveryHTTPPolicyTransientAndPermanentClassification(t *testing.T) {
	for _, status := range []int{403, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var admissions int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/oauth/transport-policy" {
					admissions++
					t.Error("bearer sent without policy opt-in")
					return
				}
				if status == 429 {
					w.Header().Set("Retry-After", "120")
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			auth := newInstantDeviceAuthClient(server.URL+"/mcp?project=brk", "asb_p_synthetic", server.Client())
			_, err := auth.exchange(t.Context(), directCodeRequest{Recovery: true})
			var temporary *directTransientError
			if errors.As(err, &temporary) != (status != 403) || admissions != 0 {
				t.Fatal("policy error classified incorrectly")
			}
			if status == 429 && temporary.retryAfter != 2*time.Minute {
				t.Fatal("policy Retry-After lost")
			}
			if status == 403 && !errors.Is(err, errHTTPTransportDenied) {
				t.Fatal("policy rejection not terminal")
			}
		})
	}
}

func TestAutoRecoveryLostDirectResponseReplaysPersistedID(t *testing.T) {
	auth := &scriptedDirect{failures: []error{&directTransientError{}}}
	now := time.Now()
	o, a, id := retryState(t, auth, devicePollClock{now: func() time.Time { return now }, sleep: func(ctx context.Context, d time.Duration) error { now = now.Add(d); return ctx.Err() }})
	if !o.reauthorize(t.Context(), a, id) || len(auth.requests) != 2 || auth.devices != 0 {
		t.Fatal("lost response did not recover direct-only")
	}
	if auth.requests[0].RequestID == "" || auth.requests[0].RequestID != auth.requests[1].RequestID {
		t.Fatal("lost response changed request ID")
	}
	saved, found, e := o.state.LoadSession(a.key)
	if e != nil || !found || saved.Direct.RequestID != auth.requests[0].RequestID || saved.Credential == nil {
		t.Fatal("replay ID/grant not durable")
	}
}
