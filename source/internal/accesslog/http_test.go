package accesslog

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() { serverdiag.RegisterRoutes("/test", "/healthz", "/mcp") }
func testLogger(t *testing.T) (*Logger, string) {
	t.Helper()
	root := t.TempDir()
	c := Defaults()
	c.File = "logs/access.log"
	l, err := Open(root, c)
	if err != nil {
		t.Fatal("log open failed")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		l.Close(ctx)
	})
	return l, filepath.Join(root, "logs", "access.log")
}
func records(t *testing.T, l *Logger, file string) []Record {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	l.Close(ctx)
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("log read failed")
	}
	var out []Record
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var e Record
		if json.Unmarshal([]byte(line), &e) != nil {
			t.Fatal("invalid log line")
		}
		if e.Type == "access" {
			out = append(out, e)
		}
	}
	return out
}
func testMeta(*http.Request) Metadata {
	return Metadata{Route: "/test", Address: Address{Peer: "127.0.0.1", Client: "127.0.0.1", Source: "peer"}}
}
func TestResponseAccountingAndAbortBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, method    string
		handler         http.HandlerFunc
		status          int
		bytes           int64
		aborted, panics bool
	}{
		{"empty", "GET", func(http.ResponseWriter, *http.Request) {}, 200, 0, false, false},
		{"write", "GET", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("abc")) }, 200, 3, false, false},
		{"early-hints", "GET", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(103)
			w.WriteHeader(201)
			w.WriteHeader(202)
		}, 201, 0, false, false},
		{"head", "HEAD", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("abc")) }, 200, 0, false, false},
		{"abort", "GET", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }, 0, 0, true, false},
		{"panic-after-header", "GET", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(202); panic("fixture-request-secret") }, 202, 0, false, true},
		{"upgrade", "GET", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(101) }, 101, 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, file := testLogger(t)
			h := l.Handler(tc.handler, testMeta)
			func() {
				defer func() {
					v := recover()
					if v != nil && v != http.ErrAbortHandler {
						t.Error("original panic escaped")
					}
				}()
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, "/", nil))
			}()
			out := records(t, l, file)
			if len(out) != 1 || out[0].Status != tc.status || out[0].Bytes != tc.bytes || out[0].Aborted != tc.aborted || out[0].Panic != tc.panics {
				t.Fatal("response accounting contract failed")
			}
			raw, _ := os.ReadFile(file)
			if strings.Contains(string(raw), "fixture-request-secret") {
				t.Fatal("panic secret reached access file")
			}
		})
	}
}

type partialWriter struct {
	http.ResponseWriter
	err error
}

func (p partialWriter) Write(b []byte) (int, error) { return 2, p.err }
func (p partialWriter) FlushError() error           { return p.err }
func TestResponseControllerAndPartialWritePreservation(t *testing.T) {
	marker := errors.New("fixture-write-error")
	base := partialWriter{httptest.NewRecorder(), marker}
	state := &responseState{}
	w := &responseWriter{base, state}
	n, err := w.Write([]byte("body"))
	if n != 2 || !errors.Is(err, marker) || state.bytes != 2 {
		t.Fatal("partial write result changed")
	}
	if !errors.Is(http.NewResponseController(w).Flush(), marker) {
		t.Fatal("FlushError was hidden")
	}
	if w.Unwrap() != base {
		t.Fatal("writer unwrap changed")
	}
}
func TestUnmatchedMethodPrivacyAndCancelledRequest(t *testing.T) {
	l, file := testLogger(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("fixture-request-secret", "/fixture-request-secret?q=fixture-request-secret", nil).WithContext(ctx)
	l.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), func(*http.Request) Metadata { return Metadata{Route: "{unmatched}"} }).ServeHTTP(httptest.NewRecorder(), r)
	out := records(t, l, file)
	if len(out) != 1 || out[0].Method != "OTHER" || out[0].PathLen == nil || *out[0].PathLen != len(r.URL.Path) || !out[0].Aborted || out[0].Status != 0 {
		t.Fatal("unmatched/aborted contract failed")
	}
	raw, _ := os.ReadFile(file)
	if strings.Contains(string(raw), "fixture-request-secret") {
		t.Fatal("unmatched raw data logged")
	}
}
