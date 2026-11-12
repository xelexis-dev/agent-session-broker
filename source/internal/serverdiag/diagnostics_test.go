package serverdiag

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type panicSecret struct{ called *atomic.Bool }

func (p panicSecret) String() string { p.called.Store(true); return "fixture-panic-secret" }
func (p panicSecret) Error() string  { return p.String() }
func TestDiagnosticPanicAndHTTPNeverFormatValues(t *testing.T) {
	var out bytes.Buffer
	s := New(&out)
	restore := Use(s)
	defer restore()
	var called atomic.Bool
	Emit(Panic(panicSecret{&called}))
	ErrorLog().Print("fixture-header-secret")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.Close(ctx)
	if called.Load() || strings.Contains(out.String(), "fixture-") || strings.Contains(out.String(), "/private/") {
		t.Fatal("raw panic/HTTP diagnostic escaped")
	}
	if !strings.Contains(out.String(), "server_request_panic") || !strings.Contains(out.String(), "http_server_diagnostic") || !strings.Contains(out.String(), "process_started_at") {
		t.Fatal("classified diagnostics missing")
	}
}

type blockedWriter struct {
	entered chan struct{}
	release chan struct{}
}

func (w blockedWriter) Write(p []byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.release
	return len(p), nil
}
func TestDiagnosticQueueAndShutdownStayBounded(t *testing.T) {
	w := blockedWriter{make(chan struct{}, 1), make(chan struct{})}
	s := New(w)
	s.Emit(Event{Type: "server-listening"})
	<-w.entered
	for range 1000 {
		s.Emit(Event{Type: "server-listening"})
	}
	if len(s.queue) != 64 || s.dropped.Load() == 0 {
		t.Fatal("diagnostic queue unbounded or losses uncounted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Close(ctx)
	s.Emit(Event{Type: "server-listening"})
	close(w.release)
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("diagnostic worker did not finish")
	}
}
func TestDiagnosticReasonAndPeerAreClassified(t *testing.T) {
	var out bytes.Buffer
	s := New(&out)
	s.Emit(Event{Type: "attachment-removed", Reason: "fixture-reason-secret", Peer: "fe80::1%fixture-zone-secret"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.Close(ctx)
	if strings.Contains(out.String(), "fixture-") {
		t.Fatal("unclassified reason/peer exposed")
	}
}

func BenchmarkDiagnosticEncoder(b *testing.B) {
	e := Event{Type: "http-transport-denied", Reason: "untrusted-forwarded", Route: "/mcp", Peer: "192.0.2.1", ProcessStartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := json.Marshal(e); err != nil {
			b.Fatal("encode failed")
		}
	}
}
func BenchmarkDiagnosticBlockedSink(b *testing.B) {
	w := blockedWriter{make(chan struct{}, 1), make(chan struct{})}
	s := New(w)
	s.Emit(Event{Type: "server-listening"})
	<-w.entered
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s.Emit(Event{Type: "server-listening"})
	}
	b.StopTimer()
	close(w.release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.Close(ctx)
}

func TestEmitRejectsUnregisteredRouteAndNonPanicFields(t *testing.T) {
	var out bytes.Buffer
	s := New(&out)
	s.Emit(Event{Type: "server-listening", Route: "fixture-private-route", PanicType: "fixture-private-type", Stack: []Frame{{Function: "fixture-private-frame"}}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.Close(ctx)
	if strings.Contains(out.String(), "fixture-private") {
		t.Fatal("unregistered route/panic fields leaked")
	}
}
func TestHTTPDiagnosticPrefixCounts(t *testing.T) {
	var out bytes.Buffer
	s := New(&out)
	restore := Use(s)
	defer restore()
	for _, raw := range []string{"http: Accept error: fixture-private", "http: Accept error: fixture-private", "http: TLS handshake error from fixture-private", "http: superfluous response.WriteHeader call from fixture-private", "http2: fixture-private", "fixture-private"} {
		ErrorLog().Print(raw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.Close(ctx)
	seen := map[string]uint64{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var e Event
		if json.Unmarshal([]byte(line), &e) != nil {
			t.Fatal("invalid diagnostic event")
		}
		seen[e.Reason] += uint64(e.Count)
	}
	for reason, want := range map[string]uint64{"accept_error": 2, "tls_handshake": 1, "superfluous_writeheader": 1, "http2": 1, "other": 1} {
		if seen[reason] != want {
			t.Error("HTTP diagnostic prefix count missing")
		}
	}
	if strings.Contains(out.String(), "fixture-private") {
		t.Fatal("raw HTTP diagnostic leaked")
	}
}
