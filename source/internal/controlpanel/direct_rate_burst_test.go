package controlpanel

import (
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func freshRateForm(n int) url.Values {
	f := directForm()
	f.Set("session_id", fmt.Sprintf("%08d-1111-4111-8111-111111111111", n))
	f.Set("request_id", fmt.Sprintf("fresh-rate-%d", n))
	return f
}

// The real exchange must allow simultaneous distinct identities, while the
// existing binding cap continues to reject an extra identity independently.
func TestDirectAutoParentConcurrentFreshBurst(t *testing.T) {
	for _, tc := range []struct{ cap, n int }{{0, 5}, {3, 3}} {
		t.Run(fmt.Sprintf("cap-%d", tc.cap), func(t *testing.T) {
			s := directStore(t)
			parent := autoRateParent(t, s, tc.cap)
			now := time.Now()
			s.now = func() time.Time { return now }
			h := New(s, tokenViewBackend{}, http.NotFoundHandler())
			var wg sync.WaitGroup
			codes := make(chan int, tc.n)
			start := make(chan struct{})
			for i := 1; i <= tc.n; i++ {
				wg.Go(func() { <-start; codes <- directPOST(t, h, parent, freshRateForm(i)).Code })
			}
			close(start)
			wg.Wait()
			close(codes)
			for code := range codes {
				if code != 200 {
					t.Errorf("concurrent fresh exchange status=%d want200", code)
				}
			}
			if tc.cap > 0 {
				if w := directPOST(t, h, parent, freshRateForm(99)); w.Code != 409 {
					t.Fatalf("cap status=%d want409", w.Code)
				}
			}
		})
	}
}

func TestDirectUnlimitedEleventhFreshExchangeRetryAfter(t *testing.T) {
	s := directStore(t)
	parent := autoRateParent(t, s, 0)
	now := time.Now()
	s.now = func() time.Time { return now }
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	for i := 1; i <= 10; i++ {
		if w := directPOST(t, h, parent, freshRateForm(i)); w.Code != 200 {
			t.Fatalf("fresh exchange %d status=%d want200", i, w.Code)
		}
	}
	w := directPOST(t, h, parent, freshRateForm(11))
	if w.Code != 429 || w.Header().Get("Retry-After") != "2" {
		t.Fatalf("11th status=%d retry=%q want429/2", w.Code, w.Header().Get("Retry-After"))
	}
	now = now.Add(1500 * time.Millisecond)
	w = directPOST(t, h, parent, freshRateForm(11))
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("partial refill status=%d retry=%q want429/1", w.Code, w.Header().Get("Retry-After"))
	}
	now = now.Add(500 * time.Millisecond)
	if w = directPOST(t, h, parent, freshRateForm(11)); w.Code != 200 {
		t.Fatalf("refilled status=%d want200", w.Code)
	}
}

func TestDirectRateDenialAuditAndPanelSurviveRestart(t *testing.T) {
	s := directStore(t)
	parent := autoRateParent(t, s, 0)
	now := time.Now()
	s.now = func() time.Time { return now }
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	if w := directPOST(t, h, parent, freshRateForm(1)); w.Code != 200 {
		t.Fatal("initial exchange failed")
	}
	p := s.data.Projects["demo"]
	s.directRates["parent/"+p.Tokens[0].ID] = directRate{Tokens: 0, At: now}
	if w := directPOST(t, h, parent, freshRateForm(2)); w.Code != 429 {
		t.Fatalf("limited status=%d want429", w.Code)
	}
	audit, err := s.DirectAuditFor("admin", "demo")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range audit {
		if e.Kind == "rejected" {
			if e.Reason != "rate-limited" {
				t.Fatalf("audit reason=%s want rate-limited", e.Reason)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("rate denial audit missing")
	}
	// Keep legacy conflicts readable alongside the new reason after reopening.
	f := freshRateForm(3)
	f.Set("requested_nickname", "broker-admin")
	if w := directPOST(t, h, parent, f); w.Code != 409 {
		t.Fatal("real conflict missing")
	}
	root := s.root.Name()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	page := loginPanelUser(t, New(reopened, tokenViewBackend{}, http.NotFoundHandler()), "admin").call("GET", "/projects/audit?project=demo", nil)
	body := page.Body.String()
	if page.Code != 200 || !strings.Contains(body, "<code>rate-limited</code>") || !strings.Contains(body, "<code>conflict</code>") {
		t.Fatal("new/legacy reason not rendered after restart")
	}
}

func autoRateParent(t *testing.T, s *Store, cap int) string {
	t.Helper()
	raw, err := s.IssueProjectTokenWithOptionsFor("admin", "demo", "rate-parent", ProjectTokenOptions{Scopes: append(access.LegacyV1Scopes(), access.SessionRegisterAuto), MaxAutoBindings: cap, UnlimitedAutoBindings: cap == 0})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDirectRateBucketBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cap, burst int
		wait       time.Duration
	}{{"unlimited", 0, 10, 2 * time.Second}, {"minimum", 1, 2, 2 * time.Second}, {"cap-two", 2, 2, 2 * time.Second}, {"cap-three", 3, 3, 2 * time.Second}, {"maximum", 10, 10, 2 * time.Second}, {"clamped", 64, 10, 2 * time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			s := &Store{now: func() time.Time { return now }}
			parent := SessionToken{ID: "parent", MaxAutoBindings: tc.cap}
			for i := 0; i < tc.burst; i++ {
				if wait := s.directRateWaitLocked("demo", parent, "peer", true); wait != 0 {
					t.Fatalf("request %d limited", i+1)
				}
			}
			if wait := s.directRateWaitLocked("demo", parent, "peer", true); wait != tc.wait {
				t.Fatalf("wait=%s want%s", wait, tc.wait)
			}
		})
	}
	for _, tc := range []struct {
		name string
		n    int
		wait time.Duration
	}{{"retry", 20, 3 * time.Second}, {"project", 60, time.Second}, {"source", 30, 2 * time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			s := &Store{now: func() time.Time { return now }}
			attempt := func(i int) time.Duration {
				parent, source := "parent", "peer"
				if tc.name != "retry" {
					parent = fmt.Sprint("parent-", i)
				}
				if tc.name == "project" {
					source = fmt.Sprint("source-", i)
				}
				return s.directRateWaitLocked("demo", SessionToken{ID: parent}, source, tc.name != "retry")
			}
			for i := 0; i < tc.n; i++ {
				if wait := attempt(i); wait != 0 {
					t.Fatalf("request %d limited", i+1)
				}
			}
			if wait := attempt(tc.n); wait != tc.wait {
				t.Fatalf("wait=%s want%s", wait, tc.wait)
			}
			now = now.Add(tc.wait)
			if wait := attempt(tc.n); wait != 0 {
				t.Fatalf("advertised refill not sufficient: %s", wait)
			}
		})
	}
}

func TestDirectRetryAfterRoundingAndLongestBucket(t *testing.T) {
	for _, tc := range []struct {
		wait   time.Duration
		header string
	}{{time.Millisecond, "1"}, {1500 * time.Millisecond, "2"}, {2 * time.Second, "2"}, {time.Minute + time.Second, "60"}} {
		w := httptest.NewRecorder()
		writeDirectFailure(w, &directRateError{wait: tc.wait})
		if w.Code != 429 || w.Header().Get("Retry-After") != tc.header || !strings.Contains(w.Body.String(), `"error":"rate_limited"`) {
			t.Fatalf("wait=%s status=%d header=%s", tc.wait, w.Code, w.Header().Get("Retry-After"))
		}
	}
	now := time.Now()
	s := &Store{now: func() time.Time { return now }, directRates: map[string]directRate{"retry/parent": {0, now}, "parent/parent": {0, now}, "project/demo": {0, now}, "source/" + digest("peer"): {0, now}}}
	if wait := s.directRateWaitLocked("demo", SessionToken{ID: "parent"}, "peer", true); wait != 3*time.Second {
		t.Fatalf("wait=%s want slowest retry bucket 3s", wait)
	}
	if s.directRates["retry/parent"].Tokens != 0 {
		t.Fatal("denial consumed a token")
	}
}

func TestDirectDenialReasonClassification(t *testing.T) {
	for _, tc := range []struct {
		err    error
		reason string
	}{{errDirectConflict, "conflict"}, {errDirectCapacity, "capacity"}, {errDirectForbidden, "forbidden"}, {errDirectUnavailable, "forbidden"}, {errDeviceClient, "parent-inactive"}, {&directRateError{wait: time.Second}, "rate-limited"}, {ErrStorage, ""}} {
		if got := directDenialReason(tc.err); got != tc.reason {
			t.Fatalf("reason=%s want%s", got, tc.reason)
		}
	}
}
