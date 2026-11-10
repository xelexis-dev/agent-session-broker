//go:build darwin || linux

package codexcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func rpcPair(t *testing.T, serve func(net.Conn)) *Client {
	t.Helper()
	a, b := net.Pipe()
	c := newClient(a)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer b.Close()
		_ = b.SetDeadline(time.Now().Add(4 * time.Second))
		serve(b)
	}()
	t.Cleanup(func() { c.Close(); b.Close(); <-done })
	return c
}
func readRequest(conn net.Conn) (map[string]any, error) {
	_, raw, err := readClientFrame(conn)
	if err != nil {
		return nil, err
	}
	var q map[string]any
	err = json.Unmarshal(raw, &q)
	return q, err
}
func reply(conn net.Conn, id any, body any) error {
	raw, _ := json.Marshal(map[string]any{"id": id, "result": body})
	return writeAll(conn, serverFrame(1, true, raw))
}
func TestRequestIDAndNotificationHandling(t *testing.T) {
	for _, kind := range []string{"notification", "wrong-id", "bad-version", "both-results", "invalid-json", "server-request"} {
		t.Run(kind, func(t *testing.T) {
			c := rpcPair(t, func(conn net.Conn) {
				q, err := readRequest(conn)
				if err != nil {
					return
				}
				id := q["id"]
				switch kind {
				case "notification":
					_ = writeAll(conn, serverFrame(1, true, []byte(`{"method":"thread/status/changed","params":{}}`)))
					_ = reply(conn, id, map[string]bool{"ok": true})
				case "wrong-id":
					_ = reply(conn, 99, map[string]bool{"wrong": true})
				case "bad-version":
					_ = writeAll(conn, serverFrame(1, true, []byte(`{"jsonrpc":"1.0","id":1,"result":{}}`)))
				case "both-results":
					_ = writeAll(conn, serverFrame(1, true, []byte(`{"id":1,"result":{},"error":{"code":-32600,"message":"fixture"}}`)))
				case "invalid-json":
					_ = writeAll(conn, serverFrame(1, true, []byte(`{`)))
				case "server-request":
					_ = writeAll(conn, serverFrame(1, true, []byte(`{"id":1,"method":"approval","params":{}}`)))
				}
			})
			raw, err := c.Call(t.Context(), "thread/turns/list", map[string]any{"threadId": "U", "limit": 1})
			if (err == nil) != (kind == "notification") {
				t.Fatal("response id/envelope not enforced")
			}
			if kind == "notification" && string(raw) != `{"ok":true}` {
				t.Fatal("notification mistaken for result")
			}
		})
	}
}
func TestMethodAllowlistAndSeparateMCPEntry(t *testing.T) {
	var calls atomic.Int32
	c := rpcPair(t, func(conn net.Conn) {
		for {
			q, err := readRequest(conn)
			if err != nil {
				return
			}
			calls.Add(1)
			if q["method"] != "mcpServer/tool/call" {
				t.Error("wrong explicit MCP method")
			}
			_ = reply(conn, q["id"], map[string]bool{"ok": true})
		}
	})
	for _, method := range []string{"turn/start", "thread/resume", "turn/interrupt", "thread/inject_items", "config/mcpServer/reload", "mcpServer/tool/call", "arbitrary"} {
		if _, err := c.Call(t.Context(), method, struct{}{}); !errors.Is(err, ErrMethod) {
			t.Fatal("forbidden method accepted", method)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("forbidden call reached wire")
	}
	if _, err := c.CallMCPTool(t.Context(), map[string]any{"threadId": "U", "server": "x-broker", "tool": "auth_status"}); err != nil || calls.Load() != 1 {
		t.Fatal("explicit MCP entry failed")
	}
}
func TestRPCRejectionDoesNotResendOrLeakMessage(t *testing.T) {
	var count atomic.Int32
	c := rpcPair(t, func(conn net.Conn) {
		_, err := readRequest(conn)
		if err != nil {
			return
		}
		count.Add(1)
		_ = writeAll(conn, serverFrame(1, true, []byte(`{"id":1,"error":{"code":-32600,"message":"private-peer-text"}}`)))
		q, err := readRequest(conn)
		if err == nil {
			count.Add(1)
			_ = reply(conn, q["id"], struct{}{})
		}
	})
	_, err := c.Call(t.Context(), "turn/steer", struct{}{})
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != -32600 || rpc.Message != "private-peer-text" || err.Error() != "codex control RPC rejected" || count.Load() != 1 {
		t.Fatal("RPC error lost or retried/leaked")
	}
	if _, err := c.Call(t.Context(), "thread/items/list", struct{}{}); err != nil || count.Load() != 2 {
		t.Fatal("explicit next call failed")
	}
}
func TestOverallDeadlineIncludesInterleavedPings(t *testing.T) {
	c := rpcPair(t, func(conn net.Conn) {
		if _, err := readRequest(conn); err != nil {
			return
		}
		for {
			if writeAll(conn, serverFrame(10, true, nil)) != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Call(ctx, "thread/turns/list", struct{}{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("deadline not enforced")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("control frames extended deadline")
	}
	if _, err := c.Call(t.Context(), "thread/turns/list", struct{}{}); !errors.Is(err, ErrClosed) {
		t.Fatal("failed connection reused")
	}
}
func TestDefaultTwoSecondRequestDeadline(t *testing.T) {
	c := rpcPair(t, func(conn net.Conn) {
		if _, err := readRequest(conn); err != nil {
			return
		}
		var b [1]byte
		_, _ = conn.Read(b[:])
	})
	start := time.Now()
	_, err := c.Call(context.Background(), "thread/turns/list", struct{}{})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatal("default request deadline missing")
	}
}
func TestCancelledReadAndConcurrentClose(t *testing.T) {
	entered := make(chan struct{})
	c := rpcPair(t, func(conn net.Conn) { _, _ = readRequest(conn); close(entered); var b [1]byte; _, _ = conn.Read(b[:]) })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := c.Call(ctx, "turn/steer", struct{}{}); done <- err }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("cancel did not interrupt read")
	}
	c.Close()
	c.Close()
}

func TestExperimentalNotificationInterleavedBeforeResponse(t *testing.T) {
	c := rpcPair(t, func(conn net.Conn) {
		for n := 0; n < 2; n++ {
			q, err := readRequest(conn)
			if err != nil {
				return
			}
			notice := []byte(`{"method":"experimental/thread/event","params":{"unused":true}}`)
			if writeAll(conn, serverFrame(1, false, notice[:20])) != nil {
				return
			}
			if writeAll(conn, serverFrame(9, true, []byte("probe"))) != nil {
				return
			}
			op, pong, err := readClientFrame(conn)
			if err != nil || op != 10 || string(pong) != "probe" {
				t.Error("interleaved ping not drained")
				return
			}
			if writeAll(conn, serverFrame(0, true, notice[20:])) != nil {
				return
			}
			if reply(conn, q["id"], map[string]int{"request": n}) != nil {
				return
			}
		}
	})
	for n := 0; n < 2; n++ {
		raw, err := c.Call(t.Context(), "thread/queue/list", map[string]any{"threadId": "U"})
		var result struct {
			Request int `json:"request"`
		}
		if err != nil || json.Unmarshal(raw, &result) != nil || result.Request != n {
			t.Fatal("notification disturbed matching or next request", err)
		}
	}
}

func TestQueueAvailabilityCachedPerControlSession(t *testing.T) {
	var queues atomic.Int32
	c := rpcPair(t, func(conn net.Conn) {
		for {
			q, err := readRequest(conn)
			if err != nil {
				return
			}
			if q["method"] == "thread/queue/list" {
				queues.Add(1)
			}
			if reply(conn, q["id"], map[string]any{"data": []any{}, "nextCursor": nil}) != nil {
				return
			}
		}
	})
	if err := c.CheckQueue(t.Context(), "U"); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckQueue(t.Context(), "U"); err != nil {
		t.Fatal(err)
	}
	if queues.Load() != 1 {
		t.Fatal("queue availability was not cached")
	}
	if _, err := c.Call(t.Context(), "thread/turns/list", map[string]any{"threadId": "U"}); err != nil {
		t.Fatal(err)
	}
}
