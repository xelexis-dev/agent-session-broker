package accesslog

import (
	"context"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"net/http"
	"sync"
	"time"
)

type Metadata struct {
	Route   string
	Address Address
	TLS     bool
}
type requestKey struct{}
type responseState struct {
	status     int
	bytes      int64
	logical    bool
	head       bool
	mu         sync.Mutex
	panicEvent *serverdiag.Event
}

var methods = map[string]string{"GET": "GET", "HEAD": "HEAD", "POST": "POST", "PUT": "PUT", "PATCH": "PATCH", "DELETE": "DELETE", "OPTIONS": "OPTIONS", "CONNECT": "CONNECT", "TRACE": "TRACE"}

func Method(method string) string {
	if canonical, ok := methods[method]; ok {
		return canonical
	}
	return "OTHER"
}

type responseWriter struct {
	http.ResponseWriter
	state *responseState
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseWriter) WriteHeader(code int) {
	if w.state.status == 0 && (code >= 200 || code == 101) {
		w.state.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *responseWriter) Write(p []byte) (int, error) {
	if w.state.status == 0 {
		w.state.status = 200
	}
	n, err := w.ResponseWriter.Write(p)
	if !w.state.logical && !w.state.head && w.state.status != 204 && w.state.status != 304 {
		w.state.bytes += int64(n)
	}
	return n, err
}
func (w *responseWriter) FlushError() error {
	if w.state.status == 0 {
		w.state.status = 200
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *responseWriter) Flush() { _ = w.FlushError() }

// LogicalBytes marks gzip input; the outer compressed write must not count twice.
func LogicalBytes(r *http.Request, n int) {
	if state, ok := r.Context().Value(requestKey{}).(*responseState); ok {
		state.logical = true
		state.bytes += int64(n)
	}
}

// RequestPanic couples a handled MCP panic to the eventual access event. When
// logging is off the existing diagnostic boundary remains independently active.
func RequestPanic(ctx context.Context, v any) {
	event := serverdiag.Panic(v)
	if state, ok := ctx.Value(requestKey{}).(*responseState); ok {
		state.mu.Lock()
		state.panicEvent = &event
		state.mu.Unlock()
		return
	}
	serverdiag.Emit(event)
}
func (l *Logger) Handler(next http.Handler, metadata func(*http.Request) Metadata) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		meta := metadata(r)
		method := Method(r.Method)
		pathLen := len(r.URL.Path)
		state := &responseState{head: r.Method == "HEAD"}
		r = r.WithContext(context.WithValue(r.Context(), requestKey{}, state))
		writer := &responseWriter{w, state}
		defer func() {
			v := recover()
			aborted := r.Context().Err() != nil || v == http.ErrAbortHandler
			state.mu.Lock()
			diagnostic := state.panicEvent
			state.mu.Unlock()
			if v != nil && v != http.ErrAbortHandler {
				event := serverdiag.Panic(v)
				diagnostic = &event
			}
			status := state.status
			if status == 0 && v == nil && !aborted {
				status = 200
			}
			event := Record{Type: "access", Time: started.UTC().Format("2006-01-02T15:04:05.000Z"), Method: method, Route: meta.Route, Status: status, DurationMS: time.Since(started).Milliseconds(), Bytes: state.bytes, Client: meta.Address.Client, ClientSource: meta.Address.Source, Peer: meta.Address.Peer, TLS: meta.TLS, ForwardedIgnored: meta.Address.Ignored, Aborted: aborted, Panic: diagnostic != nil, diagnostic: diagnostic}
			if event.Route == "{unmatched}" || event.Route == "/api/{unmatched}" {
				n := pathLen
				event.PathLen = &n
			}
			l.Submit(event)
			if v != nil {
				panic(http.ErrAbortHandler)
			}
		}()
		next.ServeHTTP(writer, r)
	})
}
