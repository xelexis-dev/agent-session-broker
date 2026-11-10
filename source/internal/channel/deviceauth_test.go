package channel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type deviceAuthServer struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	requests []*http.Request
	forms    []map[string]string
	code     func(w http.ResponseWriter, r *http.Request)
	token    func(w http.ResponseWriter, r *http.Request)
}

func newDeviceAuthServer(t *testing.T) *deviceAuthServer {
	t.Helper()
	s := &deviceAuthServer{t: t}
	mux := http.NewServeMux()
	record := func(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			form := map[string]string{}
			for key := range r.PostForm {
				form[key] = r.PostForm.Get(key)
			}
			s.mu.Lock()
			s.requests = append(s.requests, r)
			s.forms = append(s.forms, form)
			s.mu.Unlock()
			next(w, r)
		}
	}
	mux.HandleFunc("/oauth/device_authorization", record(func(w http.ResponseWriter, r *http.Request) { s.codeHandler()(w, r) }))
	mux.HandleFunc("/oauth/token", record(func(w http.ResponseWriter, r *http.Request) { s.tokenHandler()(w, r) }))
	s.server = httptest.NewTLSServer(mux)
	t.Cleanup(s.server.Close)
	s.setCode(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"device_code": "dev-secret", "user_code": "WDJB-MJHT", "verification_uri": "/auth/code", "expires_in": 600, "interval": 5})
	})
	s.setToken(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 400, map[string]any{"error": "authorization_pending"})
	})
	return s
}

// Handler fields are swapped by tests while server goroutines may still be
// serving a late request; every access goes through the mutex.
func (s *deviceAuthServer) setCode(h func(http.ResponseWriter, *http.Request)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.code = h
}

func (s *deviceAuthServer) setToken(h func(http.ResponseWriter, *http.Request)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = h
}

func (s *deviceAuthServer) codeHandler() func(http.ResponseWriter, *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code
}

func (s *deviceAuthServer) tokenHandler() func(http.ResponseWriter, *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *deviceAuthServer) client() *deviceAuthClient {
	return newInstantDeviceAuthClient(s.server.URL+"/mcp?project=brk", "asb_p_project-token", s.server.Client())
}

func (s *deviceAuthServer) last() (*http.Request, map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[len(s.requests)-1], s.forms[len(s.forms)-1]
}

func TestDeviceCodeRequestFollowsContractAndResolvesRelativeVerificationURI(t *testing.T) {
	s := newDeviceAuthServer(t)
	got, err := s.client().requestCode(context.Background(), deviceCodeRequest{Harness: "claude", SessionID: "11111111-1111-4111-8111-111111111111", ClientID: "pc-one", RequestID: "req-1"})
	if err != nil {
		t.Fatal(err)
	}
	r, form := s.last()
	if r.Method != http.MethodPost || r.URL.Path != "/oauth/device_authorization" || r.URL.Query().Get("project") != "brk" {
		t.Fatalf("request line: %s %s", r.Method, r.URL)
	}
	if r.Header.Get("Authorization") != "Bearer asb_p_project-token" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		t.Fatalf("headers: %v", r.Header)
	}
	want := map[string]string{"client_id": "asb-client-v1", "harness": "claude", "session_id": "11111111-1111-4111-8111-111111111111", "asb_client_instance_id": "pc-one", "request_id": "req-1", "scope": "asb:session"}
	for key, value := range want {
		if form[key] != value {
			t.Fatalf("form %s = %q, want %q (form %v)", key, form[key], value, form)
		}
	}
	if got.DeviceCode != "dev-secret" || got.UserCode != "WDJB-MJHT" || got.ExpiresIn != 600 || got.Interval != 5 {
		t.Fatalf("parsed authorization: %+v", got)
	}
	if got.VerificationURI != s.server.URL+"/auth/code" {
		t.Fatalf("relative verification_uri not resolved against the broker origin: %q", got.VerificationURI)
	}
}

func TestDeviceCodeRequestRejectsForeignURIOldServerAndBadToken(t *testing.T) {
	s := newDeviceAuthServer(t)
	request := deviceCodeRequest{Harness: "codex", SessionID: "22222222-2222-4222-8222-222222222222", ClientID: "pc-one", RequestID: "req-2"}
	s.setCode(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"device_code": "d", "user_code": "u", "verification_uri": "https://evil.example/auth/code", "expires_in": 600, "interval": 5})
	})
	if _, err := s.client().requestCode(context.Background(), request); err == nil {
		t.Fatal("verification_uri on another origin accepted")
	}
	s.setCode(func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) })
	if _, err := s.client().requestCode(context.Background(), request); !errors.Is(err, errServerUpgradeRequired) {
		t.Fatalf("old server not recognised: %v", err)
	}
	s.setCode(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 401, map[string]any{"error": "invalid_client"})
	})
	if _, err := s.client().requestCode(context.Background(), request); !errors.Is(err, errProjectTokenRejected) {
		t.Fatalf("rejected project token not recognised: %v", err)
	}
	s.setCode(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"device_code": "d", "user_code": "u", "verification_uri": "/auth/code", "expires_in": 0, "interval": 5})
	})
	if _, err := s.client().requestCode(context.Background(), request); err == nil {
		t.Fatal("authorization without a lifetime accepted")
	}
}

func TestRedeemMapsTokenEndpointOutcomes(t *testing.T) {
	s := newDeviceAuthServer(t)
	cases := []struct {
		status int
		body   map[string]any
		want   tokenOutcome
	}{
		{400, map[string]any{"error": "authorization_pending"}, tokenPending},
		{400, map[string]any{"error": "slow_down"}, tokenSlowDown},
		{400, map[string]any{"error": "access_denied"}, tokenDenied},
		{400, map[string]any{"error": "expired_token"}, tokenExpired},
		{400, map[string]any{"error": "invalid_grant"}, tokenInvalidGrant},
		{401, map[string]any{"error": "invalid_client"}, tokenInvalidClient},
		{503, map[string]any{"error": "server_error"}, tokenServerError},
		{200, map[string]any{"access_token": "asb_s_secret", "token_type": "Bearer", "expires_in": 2592000, "session_grant_id": "grant-1", "nickname": "agent-a"}, tokenGranted},
	}
	for _, tc := range cases {
		s.setToken(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, tc.status, tc.body) })
		grant, outcome, err := s.client().redeem(context.Background(), "dev-secret")
		if outcome != tc.want {
			t.Fatalf("%d %v: outcome %v err %v", tc.status, tc.body, outcome, err)
		}
		if tc.want == tokenGranted && (err != nil || grant.AccessToken != "asb_s_secret" || grant.Nickname != "agent-a" || grant.SessionGrantID != "grant-1" || grant.ExpiresIn != 2592000) {
			t.Fatalf("granted credential lost fields: %+v %v", grant, err)
		}
		r, form := s.last()
		if r.URL.Path != "/oauth/token" || r.URL.Query().Get("project") != "brk" || r.Header.Get("Authorization") != "Bearer asb_p_project-token" {
			t.Fatalf("token request: %s %v", r.URL, r.Header)
		}
		if form["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" || form["device_code"] != "dev-secret" || form["client_id"] != "asb-client-v1" {
			t.Fatalf("token form: %v", form)
		}
	}
	s.setToken(func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) })
	if _, outcome, err := s.client().redeem(context.Background(), "dev-secret"); outcome != tokenServerError || !errors.Is(err, errServerUpgradeRequired) {
		t.Fatalf("old server on token endpoint: %v %v", outcome, err)
	}
	s.setToken(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"access_token": "", "token_type": "Bearer"})
	})
	if _, outcome, _ := s.client().redeem(context.Background(), "dev-secret"); outcome != tokenServerError {
		t.Fatalf("empty credential treated as granted: %v", outcome)
	}
}

func TestHTTPPolicyDenialRemainsRetryableButInvalidClientDoesNot(t *testing.T) {
	s := newDeviceAuthServer(t)
	request := deviceCodeRequest{Harness: "codex", SessionID: "22222222-2222-4222-8222-222222222222", ClientID: "pc-one", RequestID: "req-keep"}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run("invalid-code-"+http.StatusText(status), func(t *testing.T) {
			s.setCode(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, status, map[string]string{"error": "invalid_client"})
			})
			if _, err := s.client().requestCode(context.Background(), request); !errors.Is(err, errProjectTokenRejected) {
				t.Fatalf("status %d code error = %v", status, err)
			}
		})
		t.Run("invalid-token-"+http.StatusText(status), func(t *testing.T) {
			s.setToken(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, status, map[string]string{"error": "invalid_client"})
			})
			if _, outcome, err := s.client().redeem(context.Background(), "device-code"); outcome != tokenInvalidClient || !errors.Is(err, errProjectTokenRejected) {
				t.Fatalf("status %d token outcome=%v error=%v", status, outcome, err)
			}
		})
	}
	s.setCode(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "http_not_allowed"})
	})
	if _, err := s.client().requestCode(context.Background(), request); err == nil || errors.Is(err, errProjectTokenRejected) {
		t.Fatalf("HTTP policy code denial was final: %v", err)
	}
	s.setToken(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "http_not_allowed"})
	})
	if _, outcome, err := s.client().redeem(context.Background(), "device-code"); outcome != tokenTransportError || err == nil || errors.Is(err, errProjectTokenRejected) {
		t.Fatalf("HTTP policy token denial outcome=%v error=%v", outcome, err)
	}
}

// scriptedRedeemer replays token endpoint answers without a network.
type scriptedRedeemer struct {
	answers []tokenOutcome
	calls   int
}

func (r *scriptedRedeemer) redeem(context.Context, string) (sessionGrant, tokenOutcome, error) {
	if r.calls >= len(r.answers) {
		return sessionGrant{}, tokenServerError, errors.New("no scripted answer")
	}
	outcome := r.answers[r.calls]
	r.calls++
	if outcome == tokenGranted {
		return sessionGrant{AccessToken: "asb_s_granted", TokenType: "Bearer", Nickname: "agent-a"}, outcome, nil
	}
	if outcome == tokenTransportError {
		return sessionGrant{}, outcome, errors.New("connection reset")
	}
	return sessionGrant{}, outcome, nil
}

func TestPollDeviceTokenHonorsSlowDownAndStopsOnFinalAnswer(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	var slept []time.Duration
	clock := devicePollClock{
		now:   func() time.Time { return now },
		sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); now = now.Add(d); return nil },
	}
	redeemer := &scriptedRedeemer{answers: []tokenOutcome{tokenPending, tokenSlowDown, tokenPending, tokenGranted}}
	grant, outcome, err := pollDeviceToken(context.Background(), redeemer, "dev-secret", 5*time.Second, now.Add(10*time.Minute), clock)
	if err != nil || outcome != tokenGranted || grant.AccessToken != "asb_s_granted" {
		t.Fatalf("poll result: %+v %v %v", grant, outcome, err)
	}
	if want := []time.Duration{5 * time.Second, 10 * time.Second, 10 * time.Second}; len(slept) != 3 || slept[0] != want[0] || slept[1] != want[1] || slept[2] != want[2] {
		t.Fatalf("intervals after slow_down: %v, want %v", slept, want)
	}
	for _, final := range []tokenOutcome{tokenDenied, tokenExpired, tokenInvalidGrant, tokenInvalidClient} {
		redeemer = &scriptedRedeemer{answers: []tokenOutcome{tokenPending, final, tokenGranted}}
		_, outcome, _ = pollDeviceToken(context.Background(), redeemer, "dev-secret", 5*time.Second, now.Add(10*time.Minute), clock)
		if outcome != final || redeemer.calls != 2 {
			t.Fatalf("final answer %v did not stop polling: outcome %v calls %d", final, outcome, redeemer.calls)
		}
	}
}

func TestPollDeviceTokenBacksOffTransportErrorsAndExpiresLocally(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	var slept []time.Duration
	clock := devicePollClock{
		now:   func() time.Time { return now },
		sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); now = now.Add(d); return nil },
	}
	redeemer := &scriptedRedeemer{answers: []tokenOutcome{tokenTransportError, tokenServerError, tokenGranted}}
	if _, outcome, err := pollDeviceToken(context.Background(), redeemer, "dev-secret", 5*time.Second, now.Add(10*time.Minute), clock); outcome != tokenGranted || err != nil {
		t.Fatalf("transient failures were final: %v %v", outcome, err)
	}
	if len(slept) != 2 || slept[0] <= 5*time.Second || slept[1] <= slept[0] {
		t.Fatalf("transport failures did not reduce polling frequency: %v", slept)
	}
	slept = nil
	redeemer = &scriptedRedeemer{answers: []tokenOutcome{tokenPending, tokenPending, tokenPending}}
	if _, outcome, _ := pollDeviceToken(context.Background(), redeemer, "dev-secret", 5*time.Second, now.Add(12*time.Second), clock); outcome != tokenExpired || redeemer.calls > 3 {
		t.Fatalf("local expiry did not end polling: %v after %d calls", outcome, redeemer.calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := pollDeviceToken(ctx, &scriptedRedeemer{answers: []tokenOutcome{tokenPending}}, "dev-secret", 5*time.Second, now.Add(time.Hour), clock); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled poll: %v", err)
	}
}
