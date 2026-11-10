package channel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRound2ClockSkewHTTPDateRetryAfter(t *testing.T) {
	for _, skew := range []time.Duration{-5 * time.Minute, 5 * time.Minute} {
		for _, mode := range []string{"direct", "recovery", "policy"} {
			t.Run(skew.String()+"/"+mode, func(t *testing.T) {
				local := time.Now().UTC().Truncate(time.Second)
				serverNow := local.Add(-skew)
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if calls.Add(1) == 1 || mode == "policy" {
						w.Header().Set("Date", serverNow.Format(http.TimeFormat))
						w.Header().Set("Retry-After", serverNow.Add(2*time.Second).Format(http.TimeFormat))
						writeJSON(w, 429, map[string]string{"error": "rate_limited"})
						return
					}
					writeDirectGrant(w, []string{"session:register"})
				}))
				defer server.Close()
				if mode == "policy" {
					guard := newHTTPPolicyGuard(server.URL+"/mcp?project=brk", server.Client())
					err := guard.ensure(t.Context())
					var temporary *httpPolicyTransientError
					if !errors.As(err, &temporary) || temporary.retryAfter != 2*time.Second {
						t.Fatal("clock skew rejected a server-relative policy retry delay")
					}
					return
				}
				client := newDeviceAuthClient(server.URL+"/mcp?project=brk", "synthetic-project", server.Client())
				// A legacy synthetic token avoids policy discovery in this isolated exchange.
				client.policy = nil
				var waits []time.Duration
				client.clock = devicePollClock{now: func() time.Time { return local }, sleep: func(ctx context.Context, d time.Duration) error { waits = append(waits, d); return ctx.Err() }}
				_, err := client.exchange(t.Context(), directCodeRequest{Recovery: mode == "recovery"})
				if mode == "recovery" {
					var temporary *directTransientError
					if !errors.As(err, &temporary) || temporary.retryAfter != 2*time.Second || calls.Load() != 1 {
						t.Fatal("clock skew converted retryable reauthorization into permanent refusal")
					}
					return
				}
				if err != nil || calls.Load() != 2 || len(waits) != 1 || waits[0] != 2*time.Second {
					t.Fatal("clock skew prevented the server-requested direct retry")
				}
			})
		}
	}
}

func TestRound2RetryReferenceMissingInvalidAndDeltaSeconds(t *testing.T) {
	local := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	for _, date := range []string{"", "invalid-date"} {
		header := http.Header{}
		header.Set("Date", date)
		if got := responseRetryReference(header, local); !got.Equal(local) {
			t.Fatal("invalid Date replaced the fallback clock")
		}
	}
	server := local.Add(-5 * time.Minute)
	header := http.Header{}
	header.Set("Date", server.Format(http.TimeFormat))
	for _, raw := range []string{"2", "20"} {
		a, e1 := recoveryRetryAfter(raw, local)
		b, e2 := recoveryRetryAfter(raw, responseRetryReference(header, local))
		if e1 != nil || e2 != nil || a != b {
			t.Fatal("server Date altered delta-seconds retry delay")
		}
	}
	if _, err := recoveryRetryAfter(server.Add(-time.Second).Format(http.TimeFormat), responseRetryReference(header, local)); err == nil {
		t.Fatal("expired server-relative retry date accepted")
	}
}

type round2PendingRedeemer struct{ calls int }

func (r *round2PendingRedeemer) redeem(context.Context, string) (sessionGrant, tokenOutcome, error) {
	r.calls++
	return sessionGrant{}, tokenPending, nil
}
func TestRound2RelativeDeviceLifetimeIgnoresConstantClockSkew(t *testing.T) {
	for _, skew := range []time.Duration{-5 * time.Minute, 5 * time.Minute} {
		now := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC).Add(skew)
		expires := now.Add(5 * time.Second)
		clock := devicePollClock{now: func() time.Time { return now }, sleep: func(ctx context.Context, d time.Duration) error { now = now.Add(d); return ctx.Err() }}
		endpoint := &round2PendingRedeemer{}
		_, outcome, err := pollDeviceToken(t.Context(), endpoint, "synthetic-device", 2*time.Second, expires, clock)
		if err != nil || outcome != tokenExpired || endpoint.calls != 3 {
			t.Fatal("constant wall-clock skew changed relative device lifetime")
		}
	}
}
