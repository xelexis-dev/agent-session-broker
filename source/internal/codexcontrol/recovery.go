package codexcontrol

import (
	"encoding/json"
	"errors"
)

// Only a positively identified non-daemon host permits a mode-switch fallback.
var ErrNotDaemonHost = errors.New("not a Codex daemon host")
var ErrQueue = errors.New("invalid Codex queue response")

type QueuePage struct {
	Count    int
	Cursor   string
	Complete bool
}

func ParseQueuePage(raw []byte) (QueuePage, error) {
	var out QueuePage
	var page struct {
		Data []json.RawMessage `json:"data"`
		Next json.RawMessage   `json:"nextCursor"`
	}
	if len(raw) > 8<<20 || json.Unmarshal(raw, &page) != nil || page.Data == nil || len(page.Next) == 0 {
		return out, ErrQueue
	}
	if string(page.Next) == "null" {
		out.Complete = true
	} else if json.Unmarshal(page.Next, &out.Cursor) != nil || out.Cursor == "" {
		return out, ErrQueue
	}
	out.Count = len(page.Data)
	return out, nil
}

var ErrTooLarge = errors.New("codex control message exceeds limit")

// RPCError retains the rejection for explicit classification. Error is safe to
// log; Message is untrusted peer data and must not be copied into diagnostics.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return "codex control RPC rejected" }
