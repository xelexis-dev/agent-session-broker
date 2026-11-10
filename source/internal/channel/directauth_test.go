package channel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDirectDiscoveryDefiniteAnswersFallBack(t *testing.T) {
	for _, tc := range []struct {
		status  int
		body    string
		wantErr bool
	}{{303, "", false}, {401, "<html>login</html>", false}, {403, "<html>login</html>", false}, {405, "", false}, {410, "", false}, {200, "<html>login</html>", false}, {404, "", false}, {200, `{"version":1,"capabilities":["scoped-session-v1"]}`, false}, {403, `{"error":"insufficient_scope"}`, false}, {200, `invalid`, false}, {302, "", false}} {
		t.Run(http.StatusText(tc.status)+tc.body[:min(len(tc.body), 8)], func(t *testing.T) {
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("public discovery carried bearer")
				}
				w.Header().Set("Location", "/mcp")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()
			c := newInstantDeviceAuthClient(s.URL+"/mcp?project=brk", "asb_p_synthetic", s.Client())
			ok, err := c.discover(t.Context())
			if ok || (err != nil) != tc.wantErr {
				t.Fatal("wrong discovery fallback classification")
			}
		})
	}
}
func TestDirectResponseLossReusesRequestAndDoesNotFollowRedirect(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		t.Run(map[bool]string{false: "loss", true: "redirect"}[redirect], func(t *testing.T) {
			var calls, escaped atomic.Int32
			var firstID string
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/oauth/session_registration" {
					escaped.Add(1)
					t.Error("bearer escaped OAuth path")
					return
				}
				n := calls.Add(1)
				id := r.PostFormValue("request_id")
				if firstID == "" {
					firstID = id
				} else if firstID != id {
					t.Error("lost response changed request ID")
				}
				if redirect {
					http.Redirect(w, r, "/mcp", http.StatusFound)
					return
				}
				if n == 1 {
					panic(http.ErrAbortHandler)
				}
				writeDirectGrant(w, []string{"session:register"})
			}))
			defer s.Close()
			c := newInstantDeviceAuthClient(s.URL+"/mcp?project=brk", "asb_p_synthetic", s.Client())
			_, err := c.exchange(t.Context(), directCodeRequest{deviceCodeRequest: deviceCodeRequest{RequestID: "synthetic-request"}})
			if redirect {
				if err == nil || calls.Load() != 1 {
					t.Fatal("redirect retried or followed")
				}
			} else if err != nil || calls.Load() != 2 {
				t.Fatal("lost response not retried once")
			}
			if escaped.Load() != 0 {
				t.Fatal("redirect escaped")
			}
		})
	}
}
func TestDirectRetryAfterAndCancellation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		value string
		want  time.Duration
		bad   bool
	}{{"10", 10 * time.Second, false}, {now.Add(20 * time.Second).Format(http.TimeFormat), 20 * time.Second, false}, {"3600", 0, true}, {"", 0, true}, {"-1", 0, true}} {
		got, err := directRetryAfter(tc.value, now)
		if got != tc.want || (err != nil) != tc.bad {
			t.Fatal("invalid Retry-After handling")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	var n atomic.Int32
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		cancel()
		w.Header().Set("Retry-After", "60")
		writeJSON(w, 429, map[string]string{"error": "rate_limited"})
	}))
	defer s.Close()
	c := newInstantDeviceAuthClient(s.URL+"/mcp?project=brk", "asb_p_synthetic", s.Client())
	start := time.Now()
	_, err := c.exchange(ctx, directCodeRequest{})
	if !errors.Is(err, context.Canceled) || time.Since(start) > time.Second || n.Load() != 1 {
		t.Fatal("exchange cancellation did not stop wait")
	}
}
func TestDirectRejectsUnknownScopeAndRawError(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 403, map[string]string{"error": "SENSITIVE-SYNTHETIC-DETAIL"})
	}))
	defer s.Close()
	c := newInstantDeviceAuthClient(s.URL+"/mcp?project=brk", "asb_p_synthetic", s.Client())
	_, err := c.exchange(t.Context(), directCodeRequest{})
	if err == nil || strings.Contains(err.Error(), "SENSITIVE") {
		t.Fatal("raw server error escaped")
	}
	g := directGrant{sessionGrant: sessionGrant{AccessToken: "asb_s_synthetic", TokenType: "Bearer", ExpiresIn: 100, SessionGrantID: "synthetic", Nickname: "worker"}, Mode: "auto", Scopes: []string{"session:register", "future:scope"}}
	if validDirectGrant(g) {
		t.Fatal("unknown scope accepted")
	}
}
func TestDirectNicknameOption(t *testing.T) {
	o, err := parseClientOptions([]string{"--broker-url", "https://broker/mcp?project=brk", "--token", "asb_p_synthetic", "--nickname", "worker-direct"})
	if err != nil || o.nickname != "worker-direct" {
		t.Fatal("nickname flag not parsed")
	}
}

func TestDirectShortRetryAfterWaitsBeforeRetry(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			writeJSON(w, 429, map[string]string{"error": "rate_limited"})
			return
		}
		writeDirectGrant(w, []string{"session:register"})
	}))
	defer s.Close()
	c := newInstantDeviceAuthClient(s.URL+"/mcp?project=brk", "asb_p_synthetic", s.Client())
	var slept time.Duration
	c.clock.sleep = func(ctx context.Context, d time.Duration) error {
		if calls.Load() != 1 {
			t.Error("retry occurred before requested wait")
		}
		slept += d
		return nil
	}
	if _, err := c.exchange(t.Context(), directCodeRequest{deviceCodeRequest: deviceCodeRequest{RequestID: "same-retry-id"}}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || slept != 2*time.Second {
		t.Fatalf("calls=%d wait=%s want2/2s", calls.Load(), slept)
	}
}
