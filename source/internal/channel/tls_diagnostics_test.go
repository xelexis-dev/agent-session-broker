package channel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type tlsDiagnosticState struct {
	state http.ConnState
	since time.Time
}
type tlsDiagnosticStep struct {
	name  string
	since time.Time
}
type tlsDiagnostics struct {
	scope            string
	mu               sync.Mutex
	connections      map[net.Conn]tlsDiagnosticState
	steps            map[string]tlsDiagnosticStep
	closed, hijacked int
	reported         bool
}

func newTLSDiagnostics() *tlsDiagnostics {
	return &tlsDiagnostics{connections: make(map[net.Conn]tlsDiagnosticState), steps: make(map[string]tlsDiagnosticStep)}
}
func (d *tlsDiagnostics) connState(c net.Conn, state http.ConnState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch state {
	case http.StateClosed:
		d.closed++
		delete(d.connections, c)
	case http.StateHijacked:
		d.hijacked++
		delete(d.connections, c)
	default:
		d.connections[c] = tlsDiagnosticState{state, time.Now()}
	}
}
func (d *tlsDiagnostics) step(lane, name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if name == "" {
		delete(d.steps, lane)
	} else {
		d.steps[lane] = tlsDiagnosticStep{name, time.Now()}
	}
}
func (d *tlsDiagnostics) report(failed bool, logf func(string, ...any)) {
	if !failed {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.reported {
		return
	}
	d.reported = true
	var counts [3]int
	var oldestActive, oldestNew time.Duration
	now := time.Now()
	for _, c := range d.connections {
		age := now.Sub(c.since)
		switch c.state {
		case http.StateNew:
			counts[0]++
			if age > oldestNew {
				oldestNew = age
			}
		case http.StateActive:
			counts[1]++
			if age > oldestActive {
				oldestActive = age
			}
		case http.StateIdle:
			counts[2]++
		}
	}
	steps := make([]string, 0, len(d.steps))
	for lane, step := range d.steps {
		steps = append(steps, fmt.Sprintf("%s:%s:%s", lane, step.name, now.Sub(step.since).Round(time.Millisecond)))
	}
	sort.Strings(steps)
	scope := d.scope
	if scope == "" {
		scope = "tls-proxy"
	}
	logf("tls-shutdown-diagnostics scope=%s new=%d active=%d idle=%d hijacked=%d closed=%d oldest_active=%s oldest_new=%s steps=[%s]", scope, counts[0], counts[1], counts[2], d.hijacked, d.closed, oldestActive.Round(time.Millisecond), oldestNew.Round(time.Millisecond), strings.Join(steps, ","))
}

func TestTLSShutdownDiagnosticsReportsStuckConnectionAndNormalRunsStayQuiet(t *testing.T) {
	d := newTLSDiagnostics()
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	d.report(false, logf)
	if len(logs) != 0 {
		t.Fatal("normal run emitted diagnostics")
	}
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(started); <-release }))
	server.Config.ConnState = d.connState
	server.StartTLS()
	t.Cleanup(server.Close)
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, err := server.Client().Get(server.URL)
		if err == nil {
			res.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("stuck TLS request did not start")
	}
	d.step("test", "injected-stuck-request")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := server.Config.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("injected shutdown: %v", err)
	}
	d.report(err != nil, logf)
	if len(logs) != 1 {
		t.Fatalf("stuck shutdown must emit one diagnostic summary; got %d", len(logs))
	}
	for _, want := range []string{"scope=tls-proxy", "new=0", "active=1", "idle=0", "hijacked=0", "closed=0", "oldest_active=", "steps=[test:injected-stuck-request:"} {
		if !strings.Contains(logs[0], want) {
			t.Fatalf("missing %q in summary %q", want, logs[0])
		}
	}
	d.report(true, logf)
	if len(logs) != 1 {
		t.Fatal("duplicate failure diagnostics")
	}
	// Shutdown timed out without relaxing its assertion; release the fixture,
	// then join its request before cleanup closes the server.
	unblock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("released request did not finish")
	}
}

func TestTLSShutdownDiagnosticsCountsStatesWithoutRetainingClosedConnections(t *testing.T) {
	d := newTLSDiagnostics()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c, e := net.Pipe()
	defer c.Close()
	defer e.Close()
	d.connState(a, http.StateNew)
	d.connState(a, http.StateActive)
	d.connState(a, http.StateIdle)
	d.connState(a, http.StateClosed)
	d.connState(c, http.StateNew)
	d.connState(c, http.StateHijacked)
	var summary string
	d.report(true, func(format string, args ...any) { summary = fmt.Sprintf(format, args...) })
	for _, want := range []string{"new=0 active=0 idle=0 hijacked=1 closed=1", "oldest_active=0s", "steps=[]"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("missing %q in %q", want, summary)
		}
	}
	if len(d.connections) != 0 {
		t.Fatal("terminal connections retained")
	}
}

// Preserve connection classes immediately before shutdown without emitting
// anything on successful runs. The copy has no addresses or request metadata.
func (d *tlsDiagnostics) snapshot() string {
	d.mu.Lock()
	copy := newTLSDiagnostics()
	for c, state := range d.connections {
		copy.connections[c] = state
	}
	for lane, step := range d.steps {
		copy.steps[lane] = step
	}
	copy.closed, copy.hijacked = d.closed, d.hijacked
	copy.scope = d.scope
	d.mu.Unlock()
	var out string
	copy.report(true, func(format string, args ...any) { out = fmt.Sprintf(format, args...) })
	return out
}
