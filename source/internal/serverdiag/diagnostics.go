// Package serverdiag is the server's bounded, value-free stderr boundary.
package serverdiag

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event contains only classified fields; it never holds requests or errors.
// Route must already be a registered constant template (controlpanel.AccessRoute).
type Event struct {
	RestartCount     uint64  `json:"restart_count,omitempty"`
	AccessLogEnabled *bool   `json:"access_log_enabled,omitempty"`
	MaxSizeBytes     int64   `json:"max_size_bytes,omitempty"`
	MaxFiles         int     `json:"max_files,omitempty"`
	MaxAgeDays       int     `json:"max_age_days,omitempty"`
	AccessDropped    uint64  `json:"dropped_total,omitempty"`
	WriteErrors      uint64  `json:"write_errors_total,omitempty"`
	RotationErrors   uint64  `json:"rotation_errors_total,omitempty"`
	PruneErrors      uint64  `json:"prune_errors_total,omitempty"`
	Reopens          uint64  `json:"reopens_total,omitempty"`
	Bind             string  `json:"bind,omitempty"`
	Detail           string  `json:"detail,omitempty"`
	Flag             string  `json:"flag,omitempty"`
	ForwardedHint    bool    `json:"-"`
	Status           string  `json:"status,omitempty"`
	Message          string  `json:"message,omitempty"`
	Type             string  `json:"type"`
	Reason           string  `json:"reason,omitempty"`
	Route            string  `json:"route,omitempty"`
	Peer             string  `json:"peer,omitempty"`
	Count            uint64  `json:"count,omitempty"`
	Targets          int     `json:"targets,omitempty"`
	Accepted         int     `json:"accepted,omitempty"`
	Failed           int     `json:"failed,omitempty"`
	Bytes            int64   `json:"bytes,omitempty"`
	PanicType        string  `json:"panic_type,omitempty"`
	Stack            []Frame `json:"stack,omitempty"`
	ProcessStartedAt string  `json:"process_started_at"`
	DroppedTotal     uint64  `json:"diagnostics_dropped_total"`
}
type Frame struct {
	Function string `json:"function"`
	File     string `json:"file"`
	Line     int    `json:"line"`
}
type item struct {
	event   Event
	barrier chan struct{}
}

// Sink owns one writer and a fixed-size queue. A blocked writer never blocks Emit.
type Sink struct {
	queue         chan item
	stop          chan struct{}
	done          chan struct{}
	stopping      atomic.Bool
	dropped       atomic.Uint64
	started       string
	httpCounts    [5]atomic.Uint64
	drainDeadline atomic.Pointer[time.Time]
}

func New(w io.Writer) *Sink {
	s := &Sink{queue: make(chan item, 64), stop: make(chan struct{}), done: make(chan struct{}), started: processStartedAt}
	StartBackground(backgroundDiagnostic, nil, func() { s.run(w) }, nil)
	return s
}

var processStartedAt = time.Now().UTC().Format(time.RFC3339Nano)
var current atomic.Pointer[Sink]
var routeMu sync.RWMutex
var registeredRoutes = map[string]bool{"{unmatched}": true, "/api/{unmatched}": true, "/assets/{file}": true, "/attachments/{id}": true}

// RegisterRoutes is called with the static dispatch inventory during package init.
func RegisterRoutes(routes ...string) {
	routeMu.Lock()
	defer routeMu.Unlock()
	for _, route := range routes {
		registeredRoutes[route] = true
	}
}

func init() { current.Store(New(os.Stderr)) }

// Use installs the process diagnostic sink. The caller owns its bounded shutdown.
func Use(s *Sink) func() { old := current.Swap(s); return func() { current.Store(old) } }
func Emit(e Event)       { current.Load().Emit(e) }
func (s *Sink) Emit(e Event) {
	if s.stopping.Load() {
		s.dropped.Add(1)
		return
	}
	switch e.Type {
	case "server_background_panic", "access_log_config", "access_log_status", "http-transport-denied", "admin-announcement", "admin-api-audit-failed", "attachment-removed", "attachment-stored", "project-deletion-warning", "registration-owner-backfill-failed", "server-setup-required", "server-trusted-proxies", "server-listening", "server-failed", "server_request_panic", "http_server_diagnostic":
	default:
		e = Event{Type: "server_diagnostic"}
	}
	if e.Route != "" {
		routeMu.RLock()
		known := registeredRoutes[e.Route]
		routeMu.RUnlock()
		if !known {
			e.Route = "{unmatched}"
		}
	}
	if e.Type != "server_request_panic" && e.Type != "server_background_panic" {
		e.PanicType = ""
		e.Stack = nil
	}
	if e.Bind != "" {
		address, err := netip.ParseAddrPort(e.Bind)
		if err != nil || address.Addr().Zone() != "" {
			e.Bind = ""
		} else {
			e.Bind = netip.AddrPortFrom(address.Addr().Unmap(), address.Port()).String()
		}
	}
	switch e.Flag {
	case "listen", "memory-dir", "trusted-proxy", "version", "access-log-file", "access-log-forwarded-for", "access-log-max-size-mib", "access-log-max-files", "access-log-max-age-days":
	default:
		e.Flag = ""
	}
	switch e.Detail {
	case "addr_in_use", "permission", "invalid_address", "path_invalid", "io", "configuration":
	default:
		e.Detail = ""
	}
	if e.Type != "server_background_panic" {
		e.RestartCount = 0
	}
	e.Message = ""
	e.Status = ""
	if e.Type == "admin-api-audit-failed" {
		e.Status = "success"
	}
	switch e.Type {
	case "server-setup-required":
		e.Message = "enter the one-time code stored in <data-dir>/control/setup-code at /setup (code not logged)"
	case "http-transport-denied":
		if e.ForwardedHint {
			e.Message = "configure --trusted-proxy and have the proxy SET X-Forwarded-Proto"
		}
	case "project-deletion-warning":
		if e.Reason == "unavailable-bugreport" {
			e.Message = "warning: project deletion skipped unavailable bug report storage"
		}
	}

	switch e.Reason {
	case "background_panic", "marked", "untrusted-forwarded", "unproven", "startup", "expired", "discard", "disabled", "send-failed", "downloaded", "ack", "project-deleted", "unavailable-bugreport", "unavailable-journal", "unreadable-journal", "undecodable-row", "journal-cleanup", "startup_failed", "shutdown_failed", "flags_invalid", "listen_failed", "data_dir_locked", "data_dir_permission", "storage_failed", "access_log_init_failed":
	default:
		e.Reason = ""
	}
	if e.Peer != "" {
		ip, err := netip.ParseAddr(e.Peer)
		if err != nil || ip.Zone() != "" {
			e.Peer = "unknown"
		} else {
			e.Peer = ip.Unmap().String()
		}
	}
	// All strings that can be input-derived are reduced by the caller before here.
	e.ProcessStartedAt = s.started
	select {
	case s.queue <- item{event: e}:
	default:
		s.dropped.Add(1)
	}
}
func (s *Sink) run(w io.Writer) {
	defer close(s.done)
	var reportHTTP func()
	write := func(v item) {
		if v.barrier != nil {
			reportHTTP()
			close(v.barrier)
			return
		}
		v.event.DroppedTotal = s.dropped.Load()
		b, err := json.Marshal(v.event)
		if err == nil {
			b = append(b, '\n')
			n, err := w.Write(b)
			if err != nil || n != len(b) {
				s.dropped.Add(1)
			}
		}
	}
	reportHTTP = func() {
		for i, reason := range httpDiagnosticReasons {
			if count := s.httpCounts[i].Swap(0); count > 0 {
				write(item{event: Event{Type: "http_server_diagnostic", Reason: reason, Count: count, ProcessStartedAt: s.started}})
			}
		}
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case v := <-s.queue:
			write(v)
		case <-ticker.C:
			reportHTTP()
		case <-s.stop:
			for {
				select {
				case v := <-s.queue:
					write(v)
				default:
					reportHTTP()
					return
				}
			}
		}
	}
}

// Flush waits for earlier events without creating an I/O goroutine per caller.
func (s *Sink) Flush(ctx context.Context) bool {
	b := make(chan struct{})
	select {
	case s.queue <- item{barrier: b}:
	case <-ctx.Done():
		return false
	case <-s.done:
		return true
	}
	select {
	case <-b:
		return true
	case <-ctx.Done():
		return false
	case <-s.done:
		return true
	}
}
func (s *Sink) Close(ctx context.Context) {
	if s.stopping.CompareAndSwap(false, true) {
		close(s.stop)
	}
	select {
	case <-s.done:
	case <-ctx.Done():
	}
}
func ProcessStartedAt() string   { return processStartedAt }
func DrainBy(deadline time.Time) { current.Load().drainDeadline.Store(&deadline) }
func Shutdown(ctx context.Context) {
	sink := current.Load()
	if d := sink.drainDeadline.Load(); d != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, *d)
		defer cancel()
	}
	sink.Close(ctx)
}
func CanonicalRoute(route string) string {
	routeMu.RLock()
	known := registeredRoutes[route]
	routeMu.RUnlock()
	if known {
		return route
	}
	return "{unmatched}"
}

// ErrorLog discards net/http's raw text, including unknown formats.
func ErrorLog() *log.Logger { return log.New(httpDiagnosticWriter{}, "", 0) }

var httpDiagnosticPrefixes = [4]string{"http: Accept error:", "http: TLS handshake error from ", "http: superfluous response.WriteHeader call from ", "http2:"}
var httpDiagnosticReasons = [5]string{"accept_error", "tls_handshake", "superfluous_writeheader", "http2", "other"}

type httpDiagnosticWriter struct{}

func (httpDiagnosticWriter) Write(p []byte) (int, error) {
	class := 4
	for i, prefix := range httpDiagnosticPrefixes {
		if bytes.HasPrefix(p, []byte(prefix)) {
			class = i
			break
		}
	}
	current.Load().httpCounts[class].Add(1)
	return len(p), nil
}

// Panic never invokes Error/String or reflect.Type.String on a dynamic type.
func Panic(v any) Event {
	e := Event{Type: "server_request_panic", PanicType: "nil"}
	if t := reflect.TypeOf(v); t != nil {
		e.PanicType = t.Kind().String()
		if t.Name() != "" && t.PkgPath() != "" {
			e.PanicType = filepath.Base(t.PkgPath()) + "." + t.Name()
		}
	}
	if len(e.PanicType) > 80 {
		e.PanicType = e.PanicType[:80]
	}
	var pcs [16]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		function := f.Function
		if i := strings.Index(function, "/source/"); i >= 0 {
			function = function[i+8:]
		}
		file := filepath.Base(f.File)
		if len(function) > 128 {
			function = function[:128]
		}
		if len(file) > 80 {
			file = file[:80]
		}
		e.Stack = append(e.Stack, Frame{Function: function, File: file, Line: f.Line})
		if !more {
			break
		}
	}
	return e
}
