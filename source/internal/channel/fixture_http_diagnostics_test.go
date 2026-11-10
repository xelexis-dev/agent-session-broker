package channel

import (
	"context"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
)

// Observe only connection lifecycle, never bytes, headers, URLs or addresses.
type fixtureDiagnosticConn struct {
	net.Conn
	diagnostics *tlsDiagnostics
	once        sync.Once
}

func (c *fixtureDiagnosticConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.diagnostics.connState(c, http.StateClosed) })
	return err
}

type fixtureDiagnosticTransport struct {
	next        *http.Transport
	diagnostics *tlsDiagnostics
}

func newFixtureDiagnosticTransport(d *tlsDiagnostics) *fixtureDiagnosticTransport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		c, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		observed := &fixtureDiagnosticConn{Conn: c, diagnostics: d}
		d.connState(observed, http.StateNew)
		return observed, nil
	}
	return &fixtureDiagnosticTransport{next: transport, diagnostics: d}
}
func (t *fixtureDiagnosticTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var mu sync.Mutex
	var conn net.Conn
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			mu.Lock()
			conn = info.Conn
			mu.Unlock()
			t.diagnostics.connState(info.Conn, http.StateActive)
		},
		PutIdleConn: func(err error) {
			if err == nil {
				mu.Lock()
				c := conn
				mu.Unlock()
				if c != nil {
					t.diagnostics.connState(c, http.StateIdle)
				}
			}
		},
	}
	return t.next.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
}
