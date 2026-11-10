//go:build darwin || linux

// Package codexcontrol connects only to the verified Codex ancestor's local
// daemon. A failed exchange closes the connection; it never reconnects or resends.
package codexcontrol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const requestTimeout = 2 * time.Second

var ErrMethod = errors.New("codex control method is not allowed")
var ErrTransport = errors.New("codex control transport failed")

type Client struct {
	queueMu      sync.Mutex
	queueChecked bool
	queueErr     error
	conn         net.Conn
	ws           websocket
	gate         chan struct{}
	next         uint64
	closed       atomic.Bool
}

func newClient(conn net.Conn) *Client {
	return &Client{conn: conn, ws: websocket{r: bufio.NewReader(conn), w: conn}, gate: make(chan struct{}, 1)}
}

// Call permits only steering/control discovery methods. MCP tools have their
// own explicit entry below, so a steering caller cannot select them by name.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	switch method {
	case "initialize", "thread/turns/list", "thread/items/list", "thread/queue/list", "turn/steer":
	default:
		return nil, ErrMethod
	}
	return c.exchange(ctx, method, params, false)
}
func (c *Client) CallMCPTool(ctx context.Context, params any) (json.RawMessage, error) {
	return c.exchange(ctx, "mcpServer/tool/call", params, false)
}
func (c *Client) initialized(ctx context.Context) error {
	_, err := c.exchange(ctx, "initialized", struct{}{}, true)
	return err
}
func (c *Client) exchange(ctx context.Context, method string, params any, notification bool) (result json.RawMessage, err error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if c.closed.Load() {
		return nil, ErrClosed
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	deadline, _ := ctx.Deadline()
	if c.conn.SetDeadline(deadline) != nil {
		c.Close()
		return nil, ErrTransport
	}
	joined := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetDeadline(time.Now()); close(joined) })
	defer func() {
		if !stop() {
			<-joined
		}
	}()
	defer func() {
		if err != nil {
			var rpc *RPCError
			if !errors.As(err, &rpc) {
				c.Close()
			}
		}
	}()
	c.next++
	request := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if !notification {
		request["id"] = c.next
	}
	raw, e := json.Marshal(request)
	if e != nil {
		return nil, ErrProtocol
	}
	if len(raw) > maxMessageBytes {
		return nil, ErrTooLarge
	}
	if e = c.ws.writeFrame(1, raw); e != nil {
		return nil, exchangeError(ctx, e)
	}
	if notification {
		return nil, nil
	}
	for {
		raw, e = c.ws.readMessage()
		if e != nil {
			return nil, exchangeError(ctx, e)
		}
		var reply struct {
			Version string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Result  json.RawMessage `json:"result"`
			Error   *RPCError       `json:"error"`
		}
		// Codex's protocol omits jsonrpc in responses; reject conflicting versions.
		if json.Unmarshal(raw, &reply) != nil || (reply.Version != "" && reply.Version != "2.0") {
			return nil, ErrProtocol
		}
		if len(reply.ID) == 0 && reply.Method != "" {
			continue
		}
		var id uint64
		if json.Unmarshal(reply.ID, &id) != nil || id != c.next || reply.Method != "" || (len(reply.Result) == 0) == (reply.Error == nil) {
			return nil, ErrProtocol
		}
		if reply.Error != nil {
			return nil, reply.Error
		}
		return reply.Result, nil
	}
}
func exchangeError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return context.DeadlineExceeded
	}
	for _, known := range []error{ErrProtocol, ErrClosed, ErrTooLarge} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrTransport
}
func (c *Client) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	select {
	case c.gate <- struct{}{}:
		_ = c.conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		_ = c.ws.writeFrame(8, []byte{3, 232})
		<-c.gate
	default:
	}
	if c.conn.Close() != nil {
		return ErrTransport
	}
	return nil
}

// The experimental read API must work before a new durable steer intent exists.
func (c *Client) CheckQueue(ctx context.Context, thread string) error {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	if c.queueChecked {
		return c.queueErr
	}
	raw, err := c.Call(ctx, "thread/queue/list", map[string]any{"threadId": thread, "limit": 1})
	if err == nil {
		_, err = ParseQueuePage(raw)
	}
	c.queueChecked = true
	c.queueErr = err
	return err
}
