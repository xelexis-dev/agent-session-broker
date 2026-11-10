package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const hookWriteKey = "xbroker-internal-hook-write"

// The marker is removed before transport Write; only its successful return
// completes the host handoff, never handler return or local queue removal.
type hookWriteCompletions struct {
	mu      sync.Mutex
	next    uint64
	pending map[uint64]hookWriteCallbacks
}

type hookWriteCallbacks struct {
	done   func() error
	failed func()
}

func (h *hookWriteCompletions) result(done func() error, failed func()) *mcp.CallToolResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	if h.pending == nil {
		h.pending = map[uint64]hookWriteCallbacks{}
	}
	h.pending[h.next] = hookWriteCallbacks{done: done, failed: failed}
	return &mcp.CallToolResult{Meta: mcp.Meta{hookWriteKey: h.next}}
}
func (h *hookWriteCompletions) finish(id uint64, written bool) {
	h.mu.Lock()
	entry := h.pending[id]
	delete(h.pending, id)
	h.mu.Unlock()
	if written {
		if entry.done != nil {
			_ = entry.done()
		}
	} else if entry.failed != nil {
		entry.failed()
	}
}

type hookWriteTransport struct {
	mcp.Transport
	completions *hookWriteCompletions
}

func (t hookWriteTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &hookWriteConnection{Connection: conn, completions: t.completions}, nil
}

type hookWriteConnection struct {
	mcp.Connection
	completions *hookWriteCompletions
}

func (c *hookWriteConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	var completionID uint64
	failed := false
	if response, ok := message.(*jsonrpc.Response); ok && bytes.Contains(response.Result, []byte(hookWriteKey)) {
		var result map[string]json.RawMessage
		if json.Unmarshal(response.Result, &result) == nil {
			var meta map[string]json.RawMessage
			if json.Unmarshal(result["_meta"], &meta) == nil {
				var id uint64
				if json.Unmarshal(meta[hookWriteKey], &id) == nil {
					completionID = id
				}
				delete(meta, hookWriteKey)
				if len(meta) == 0 {
					delete(result, "_meta")
				} else {
					result["_meta"], _ = json.Marshal(meta)
				}
				copy := *response
				copy.Result, _ = json.Marshal(result)
				message = &copy
				if response.Error != nil || string(result["isError"]) == "true" {
					failed = true
				}
			}
		}
	}
	err := c.Connection.Write(ctx, message)
	if completionID != 0 {
		c.completions.finish(completionID, err == nil && !failed)
	}
	return err
}
func (c *hookWriteConnection) Close() error {
	c.completions.mu.Lock()
	pending := c.completions.pending
	c.completions.pending = nil
	c.completions.mu.Unlock()
	for _, entry := range pending {
		if entry.failed != nil {
			entry.failed()
		}
	}
	return c.Connection.Close()
}
