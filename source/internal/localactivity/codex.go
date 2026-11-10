package localactivity

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"strings"
	"time"
)

const metaLimit = 64 << 10
const anchorLimit = 4096

func (b *readBudget) readSessionMeta(reader io.ReaderAt, size int64) ([]byte, error) {
	// Grow from one KiB only when the metadata newline is absent,
	// keeping the existing 64 KiB limit and counting every byte in the budget.
	end := min(size, int64(1024))
	var prefix []byte
	for {
		start := len(prefix)
		chunk, err := b.readAt(reader, int64(start), end-int64(start))
		if err != nil {
			return nil, err
		}
		if prefix == nil {
			prefix = chunk
		} else {
			prefix = append(prefix, chunk...)
		}
		if cut := bytes.IndexByte(chunk, '\n'); cut >= 0 {
			return prefix[:start+cut], nil
		}
		if len(prefix) >= metaLimit {
			return nil, budgetExceeded
		}
		if int64(len(prefix)) >= size {
			return nil, malformed
		}
		end = min(2*end, size, metaLimit)
	}
}

// Only selected metadata, hashed turn identity and an offset/hash are cached.
type rolloutState struct {
	model, effort        string
	offset               int64
	metaHash, anchorHash [32]byte
	anchorStart          int64
	anchorLen            int
	turn, done           [32]byte
	hasTurn              bool
	state                string
	changed, progress    time.Time
	lastEvent            time.Time
	reason               evidenceError
}

func (o *Observer) observeCodex(b *readBudget, now time.Time) (string, time.Time, error) {
	path, err := o.cachedRollout(b.ctx, now)
	if err != nil {
		return "", time.Time{}, err
	}
	f, err := o.openEvidence(path)
	if err != nil {
		return "", time.Time{}, err
	}
	defer f.close()
	meta, err := b.readSessionMeta(f.file, f.info.Size())
	if err != nil {
		return "", time.Time{}, err
	}
	var envelope struct {
		Type    string `json:"type"`
		Payload struct {
			ID        string `json:"id"`
			SessionID string `json:"session_id"`
			CWD       string `json:"cwd"`
			Host      string `json:"hostname"`
		} `json:"payload"`
	}
	if json.Unmarshal(meta, &envelope) != nil || envelope.Type != "session_meta" {
		return "", time.Time{}, malformed
	}
	if envelope.Payload.ID != o.config.Binding.SessionID || (envelope.Payload.SessionID != "" && envelope.Payload.SessionID != o.config.Binding.SessionID) || !samePath(envelope.Payload.CWD, o.config.CWD) || (envelope.Payload.Host != "" && !strings.EqualFold(envelope.Payload.Host, o.config.Host)) {
		return "", time.Time{}, mismatch
	}
	hash := sha256.Sum256(meta)
	if o.rollout.offset > 0 && hash != o.rollout.metaHash {
		o.blocked = true
		return "", time.Time{}, malformed
	}
	if o.rollout.anchorLen > 0 {
		anchor, err := b.readAt(f.file, o.rollout.anchorStart, int64(o.rollout.anchorLen))
		if err != nil {
			return "", time.Time{}, err
		}
		if sha256.Sum256(anchor) != o.rollout.anchorHash {
			o.blocked = true
			return "", time.Time{}, malformed
		}
	}
	start := o.rollout.offset
	partialStart := false
	if start == 0 {
		start = int64(len(meta) + 1)
		// Initial history is a bounded tail, not an unbounded whole-file scan.
		// Leave one byte for checking whether the tail starts inside a line.
		if f.info.Size()-start > b.left-1 {
			start = f.info.Size() - (b.left - 1)
			previous, err := b.readAt(f.file, start-1, 1)
			if err != nil {
				return "", time.Time{}, err
			}
			partialStart = previous[0] != '\n'
		}
		o.rollout = rolloutState{metaHash: hash}
	}
	data, err := b.readAt(f.file, start, f.info.Size()-start)
	if err != nil {
		return "", time.Time{}, err
	}
	pos := 0
	if partialStart {
		n := bytes.IndexByte(data, '\n')
		if n < 0 {
			return "", time.Time{}, budgetExceeded
		}
		if n+1 > maxRead {
			return "", time.Time{}, budgetExceeded
		}
		pos = n + 1
	}
	for pos < len(data) {
		if b.ctx.Err() != nil {
			return "", time.Time{}, unavailable
		}
		n := bytes.IndexByte(data[pos:], '\n')
		if n < 0 {
			if len(data)-pos >= maxRead {
				return "", time.Time{}, budgetExceeded
			}
			return "", time.Time{}, malformed
		}
		if n+1 > maxRead {
			return "", time.Time{}, budgetExceeded
		}
		if err := o.applyLine(data[pos:pos+n], now); err != nil {
			return "", time.Time{}, err
		}
		pos += n + 1
	}
	if err = f.validate(); err != nil {
		return "", time.Time{}, err
	}
	o.runtimeVerified = true
	o.rollout.offset = f.info.Size()
	if len(data) > 0 {
		length := min(len(data), anchorLimit)
		o.rollout.anchorLen = length
		o.rollout.anchorStart = f.info.Size() - int64(length)
		o.rollout.anchorHash = sha256.Sum256(data[len(data)-length:])
	}
	if o.rollout.reason != "" {
		return "", time.Time{}, o.rollout.reason
	}
	if o.rollout.state == "" {
		return "", time.Time{}, missing
	}
	if o.rollout.state == Working && !now.Before(o.rollout.progress.Add(staleAfter)) {
		return "", time.Time{}, stale
	}
	return o.rollout.state, o.rollout.changed, nil
}

func (o *Observer) applyLine(line []byte, now time.Time) error {
	var event struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Payload   struct {
			Model       json.RawMessage `json:"model"`
			Effort      json.RawMessage `json:"effort"`
			Type        string          `json:"type"`
			TurnID      string          `json:"turn_id"`
			StartedAt   int64           `json:"started_at"`
			CompletedAt int64           `json:"completed_at"`
		} `json:"payload"`
	}
	if len(line) == 0 || json.Unmarshal(line, &event) != nil || len(event.Type) > 64 || len(event.Payload.Type) > 64 {
		return malformed
	}
	if event.Type == "turn_context" {
		o.rollout.model, o.rollout.effort = selectedRuntimeJSON(event.Payload.Model, event.Payload.Effort)
	}
	if o.config.Format == CodexTaskPairV1 {
		return o.applyTaskPair(event.Type, event.Payload.Type, event.Payload.TurnID, event.Timestamp, now)
	}
	if event.Type != "event_msg" {
		return nil
	}
	kind := event.Payload.Type
	if kind != "task_started" && kind != "task_complete" && kind != "turn_aborted" && kind != "tool_started" && kind != "tool_complete" && kind != "task_progress" {
		if strings.HasPrefix(kind, "task_") || strings.HasPrefix(kind, "turn_") {
			return malformed
		}
		return nil // Message bodies and mtime are never progress evidence.
	}
	turn, ok := turnKey(event.Payload.TurnID)
	if !ok {
		return malformed
	}
	at, err := time.Parse(time.RFC3339Nano, event.Timestamp)
	seconds := event.Payload.StartedAt
	if kind == "task_complete" || kind == "turn_aborted" {
		seconds = event.Payload.CompletedAt
	}
	if seconds > 0 {
		at = time.Unix(seconds, 0)
		err = nil
	}
	if err != nil || !trustedTime(at, now) {
		return malformed
	}
	at = at.UTC()
	if !o.rollout.progress.IsZero() && at.Before(o.rollout.progress) {
		return malformed
	}
	switch kind {
	case "task_started":
		if o.rollout.hasTurn {
			return malformed
		}
		o.rollout.turn, o.rollout.hasTurn, o.rollout.state, o.rollout.changed, o.rollout.progress = turn, true, Working, at, at
	case "task_complete", "turn_aborted":
		if !o.rollout.hasTurn || turn != o.rollout.turn {
			return malformed
		}
		o.rollout.hasTurn, o.rollout.state, o.rollout.changed, o.rollout.progress = false, Idle, at, at
	default:
		if !o.rollout.hasTurn || turn != o.rollout.turn {
			return malformed
		}
		o.rollout.progress = at
	}
	return nil
}

// Selected response items may inherit exactly one open turn in this verified
// rollout. Bookkeeping preserves the pair without extending its progress timer.
// Explicit IDs must match; completion still requires an explicit matching ID.
func (o *Observer) applyTaskPair(envelope, kind, id, timestamp string, now time.Time) error {
	unknown := func(forgetPair bool) error {
		if forgetPair {
			o.rollout.hasTurn = false
			o.rollout.turn = [32]byte{}
			o.rollout.progress = time.Time{}
		}
		o.rollout.state = Unknown
		o.rollout.changed = time.Time{}
		o.rollout.reason = malformed
		return nil
	}
	action := ""
	inherit := false
	switch envelope {
	case "turn_context", "token_usage_record", "compacted", "world_state":
		action = "neutral"
	case "event_msg":
		switch kind {
		case "token_count", "item_completed", "thread_settings_applied":
			action = "neutral"
		case "task_started", "task_complete", "turn_aborted":
			action = kind
		case "agent_message", "agent_reasoning", "exec_command_begin", "exec_command_end", "mcp_tool_call_begin", "mcp_tool_call_end":
			action = "progress"
		case "exec_approval_request", "apply_patch_approval_request", "permission_request", "request_permissions":
			action = "approval"
		}
	case "response_item":
		switch kind {
		case "additional_tools":
			action = "neutral"
		case "message", "reasoning", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output":
			action = "progress"
			inherit = true
		}
	}
	at, err := time.Parse(time.RFC3339Nano, timestamp)
	if action == "" || err != nil || !trustedTime(at, now) {
		return unknown(true)
	}
	turn, ok := turnKey(id)
	if id == "" && (inherit || action == "neutral") && o.rollout.hasTurn {
		turn, ok = o.rollout.turn, true
	}
	if !ok {
		return unknown(true)
	}
	// Late bookkeeping for the last completed turn must not touch the live pair or its clocks.
	if id != "" && turn == o.rollout.done && (envelope == "token_usage_record" ||
		(envelope == "event_msg" && (kind == "item_completed" || kind == "token_count"))) {
		return nil
	}
	if !o.rollout.lastEvent.IsZero() && at.Before(o.rollout.lastEvent) {
		return unknown(true)
	}
	o.rollout.lastEvent = at.UTC()
	if action == "neutral" {
		if !o.rollout.hasTurn || turn != o.rollout.turn {
			return unknown(true)
		}
		return nil
	}
	if action == "task_started" {
		if o.rollout.hasTurn {
			return unknown(true)
		}
		o.rollout.turn, o.rollout.hasTurn, o.rollout.state = turn, true, Working
	} else {
		if !o.rollout.hasTurn || turn != o.rollout.turn {
			return unknown(true)
		}
		switch action {
		case "task_complete":
			o.rollout.done = turn
			o.rollout.hasTurn, o.rollout.state = false, Idle
		case "turn_aborted":
			return unknown(true)
		case "approval":
			return unknown(false)
		case "progress":
			// Silence expires the display evidence, not the verified open pair.
			// A new ordered same-turn event is fresh progress even after a gap.
			// Completed, aborted or invalidated pairs were rejected above.
			o.rollout.state = Working
		}
	}
	o.rollout.changed, o.rollout.progress, o.rollout.reason = at.UTC(), at.UTC(), ""
	return nil
}
