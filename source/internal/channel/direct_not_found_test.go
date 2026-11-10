package channel

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestDirectNotFoundAndIdentityConflictKeepDistinctStopReasons(t *testing.T) {
	for _, tc := range []struct{ body, reason string }{
		{`{"error":"identity_conflict","handover_reason":"not-found"}`, "registration-not-found"},
		{`{"error":"identity_conflict"}`, "identity-conflict"},
		{`{"error":"identity_conflict","handover_reason":"owner-mismatch"}`, "registration-owner-mismatch"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(409)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()
			c := newDeviceAuthClient(s.URL+"/mcp?project=fixture", "synthetic-project", s.Client())
			c.policy = nil
			_, err := c.exchange(t.Context(), directCodeRequest{Recovery: true})
			if recoveryStopReason(err) != tc.reason || calls.Load() != 1 {
				t.Fatal("permanent direct refusal lost classification or retried")
			}
		})
	}
}
