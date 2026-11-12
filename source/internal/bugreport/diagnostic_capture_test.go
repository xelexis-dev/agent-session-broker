package bugreport

import (
	"bytes"
	"context"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"sync"
	"testing"
	"time"
)

type diagnosticCapture struct {
	mu   sync.Mutex
	b    bytes.Buffer
	sink *serverdiag.Sink
}

func (d *diagnosticCapture) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.b.Write(p)
}
func (d *diagnosticCapture) String() string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d.sink.Flush(ctx)
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.b.String()
}
func captureDiagnostics(t *testing.T) *diagnosticCapture {
	t.Helper()
	d := &diagnosticCapture{}
	d.sink = serverdiag.New(d)
	restore := serverdiag.Use(d.sink)
	t.Cleanup(func() {
		restore()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		d.sink.Close(ctx)
	})
	return d
}
