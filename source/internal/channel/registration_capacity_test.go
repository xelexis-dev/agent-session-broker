package channel

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAttachSessionDirectCapacityGuidance(t *testing.T) {
	var exchanges atomic.Int32
	f := directFixture(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		writeJSON(w, http.StatusConflict, map[string]string{"error": "capacity_exceeded"})
	}, func(f *deviceModeFixture) { f.deferAttach = true })
	result := f.call("", "attach_session", map[string]any{})
	raw, _ := json.Marshal(result)
	text := string(raw)
	if !result.IsError || !strings.Contains(text, "registration-capacity") || !strings.Contains(text, "administrator") || !strings.Contains(text, "detach unused sessions") || !strings.Contains(text, "limit") || !strings.Contains(text, "attach_session again") {
		t.Fatal("attach_session did not explain the token registration limit and administrator action")
	}
	if exchanges.Load() != 1 || f.codeRequests() != 0 || f.tokenRequests() != 0 || f.log.count("register_session") != 0 {
		t.Fatal("capacity triggered retries, device fallback or registration")
	}
}

func TestDirectCapacity409ClassificationPreservesConflictsAndHandover(t *testing.T) {
	for _, tc := range []struct {
		name, body         string
		handover, capacity bool
	}{
		{"capacity", `{"error":"capacity_exceeded"}`, false, true},
		{"identity", `{"error":"identity_conflict"}`, false, false},
		{"unknown", `{"error":"future_error"}`, false, false},
		{"empty-object", `{}`, false, false},
		{"invalid-json", `invalid`, false, false},
		{"wrong-type", `{"error":42}`, false, false},
		{"handover", `{"error":"identity_conflict","handover_reason":"still-receiving"}`, true, false},
		{"handover-capacity", `{"error":"capacity_exceeded","handover_reason":"owner-mismatch"}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(409)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			c := newDeviceAuthClient(server.URL+"/mcp?project=brk", "synthetic-project", server.Client())
			c.policy = nil
			c.clock.sleep = func(context.Context, time.Duration) error { t.Fatal("permanent 409 retried"); return nil }
			_, err := c.exchange(t.Context(), directCodeRequest{Handover: tc.handover})
			if tc.handover {
				var refusal broker.HandoverRefusal
				if !errors.As(err, &refusal) {
					t.Fatal("handover refusal path changed")
				}
				expected := "still-receiving"
				if tc.name == "handover-capacity" {
					expected = "owner-mismatch"
				}
				if refusal.Reason != expected {
					t.Fatal("handover reason changed")
				}
			} else if tc.capacity {
				if !errors.Is(err, errDirectCapacity) {
					t.Fatal("capacity not classified")
				}
			} else if !errors.Is(err, errDirectConflict) && tc.name != "invalid-json" && tc.name != "wrong-type" {
				t.Fatal("ordinary conflict changed")
			} else if (tc.name == "invalid-json" || tc.name == "wrong-type") && !errors.Is(err, errDirectConflict) {
				t.Fatal("unknown 409 body did not retain conflict guidance")
			}
			if calls.Load() != 1 {
				t.Fatal("409 retried")
			}
		})
	}
}

func TestCapacityRecoveryStopsWithoutRetryAndPersistsDiagnostic(t *testing.T) {
	auth := &scriptedDirect{failures: []error{errDirectCapacity}}
	sleeps := 0
	opts, a, id := retryState(t, auth, devicePollClock{now: time.Now, sleep: func(context.Context, time.Duration) error { sleeps++; return nil }})
	// retryState uses a synthetic key; derive a real session key so diagnostics
	// are validated and persisted just as they are for a running client.
	a.state.SessionID = id.SessionID
	a.state.Registration.RegistrationID = "AAAAAAAAAAAAAAAAAAAAAAAAAA"
	a.key = clientstate.SessionKey("capacity-test", id.Harness, id.SessionID, "synthetic-client")
	if opts.reauthorize(t.Context(), a, id) || a.state.AutoRecovery.StopReason != "registration-capacity" || sleeps != 0 || len(auth.requests) != 1 || auth.devices != 0 {
		t.Fatal("capacity recovery did not stop permanently")
	}
	if !strings.Contains(a.message, registrationCapacityGuidance) {
		t.Fatal("recovery lost administrator guidance")
	}
	if opts.reauthorize(t.Context(), a, id) || len(auth.requests) != 1 {
		t.Fatal("stopped capacity recovery retried")
	}
	d, err := opts.state.LoadDiagnostics(a.key)
	if err != nil || d.Summary.LastFailureCode != "capacity" {
		t.Fatal("capacity diagnostic was lost")
	}
}
