package channel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type devicePollOutcome struct {
	Start, End time.Duration
	Status     int
	Code       string
}
type devicePollTrace struct {
	mu                  sync.Mutex
	approved            time.Time
	outcomes            []devicePollOutcome
	pending, grant      chan struct{}
	failures, slowdowns int
	excessive           bool
}

func newDevicePollTrace() *devicePollTrace {
	return &devicePollTrace{approved: time.Now(), pending: make(chan struct{})}
}
func (d *devicePollTrace) arm(failures int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.approved = time.Now()
	d.failures = failures
	d.grant = make(chan struct{})
	d.slowdowns = 0
	d.excessive = false
}
func (d *devicePollTrace) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if r.URL.Path != "/oauth/token" {
		next.ServeHTTP(w, r)
		return
	}
	start := time.Now()
	d.mu.Lock()
	inject := d.failures > 0
	if inject {
		d.failures--
	}
	reference := d.approved
	d.mu.Unlock()
	rec := httptest.NewRecorder()
	if inject {
		rec.Header().Set("Content-Type", "application/json")
		rec.WriteHeader(503)
		io.WriteString(rec, `{"error":"server_error"}`)
	} else {
		next.ServeHTTP(rec, r)
	}
	code := "other"
	if rec.Code == 200 {
		code = "granted"
	} else {
		var body struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		switch body.Error {
		case "authorization_pending", "slow_down", "server_error", "access_denied", "expired_token", "invalid_grant", "invalid_client":
			code = body.Error
		}
	}
	d.mu.Lock()
	d.outcomes = append(d.outcomes, devicePollOutcome{start.Sub(reference).Round(time.Millisecond), time.Since(reference).Round(time.Millisecond), rec.Code, code})
	if code == "authorization_pending" && d.pending != nil {
		close(d.pending)
		d.pending = nil
	}
	if d.grant != nil {
		if code == "slow_down" {
			d.slowdowns++
			if d.slowdowns > 2 {
				d.excessive = true
			}
		} else {
			d.slowdowns = 0
		}
		if code == "granted" {
			close(d.grant)
			d.grant = nil
		}
	}
	d.mu.Unlock()
	for key, values := range rec.Header() {
		w.Header()[key] = values
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}
func (d *devicePollTrace) waitPending(ctx context.Context) bool {
	d.mu.Lock()
	ch := d.pending
	d.mu.Unlock()
	if ch == nil {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-ch:
		return true
	}
}
func (d *devicePollTrace) waitGrant(ctx context.Context) bool {
	d.mu.Lock()
	ch := d.grant
	d.mu.Unlock()
	if ch == nil {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-ch:
		return true
	}
}
func (d *devicePollTrace) check(t *testing.T) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.excessive {
		t.Error("more than two consecutive slow_down responses after approval")
	}
}
func (d *devicePollTrace) report(t *testing.T) {
	if !t.Failed() {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, out := range d.outcomes {
		t.Logf("device-poll start=%s end=%s status=%d code=%s", out.Start, out.End, out.Status, out.Code)
	}
}
