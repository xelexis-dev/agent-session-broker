package localactivity

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"path/filepath"
	"regexp"

	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func selectedRuntimeJSON(modelJSON, effortJSON json.RawMessage) (string, string) {
	var model, effort string
	_ = json.Unmarshal(modelJSON, &model)
	_ = json.Unmarshal(effortJSON, &effort)
	return selectedRuntimeLabels(model, effort)
}

func selectedRuntimeLabels(model, effort string) (string, string) {
	if !protocol.ValidRuntimeLabel(model, 96) {
		model = ""
	}
	if !protocol.ValidRuntimeLabel(effort, 16) {
		effort = ""
	}
	return model, effort
}

type transcriptState struct {
	offset        int64
	anchorStart   int64
	anchorLen     int
	anchorHash    [32]byte
	model, effort string
}

var claudeSlugChars = regexp.MustCompile(`[^a-zA-Z0-9]`)

// Exact bound conversation only. No directory scan, parent transcript, message
// content, environment fallback or model argument can select a different file.
func (o *Observer) observeTranscript(b *readBudget) error {
	path := filepath.Join("projects", claudeSlugChars.ReplaceAllString(o.config.CWD, "-"), o.config.Binding.SessionID+".jsonl")
	f, err := o.openEvidence(path)
	if err != nil {
		return err
	}
	defer f.close()
	state := o.transcript
	if f.info.Size() < state.offset {
		o.blocked = true
		return malformed
	}
	if state.anchorLen > 0 {
		anchor, err := b.readAt(f.file, state.anchorStart, int64(state.anchorLen))
		if err != nil {
			return err
		}
		if sha256.Sum256(anchor) != state.anchorHash {
			o.blocked = true
			return malformed
		}
	}
	start := state.offset
	partial := false
	if start == 0 && f.info.Size() > b.left-1 {
		start = f.info.Size() - (b.left - 1)
		prev, err := b.readAt(f.file, start-1, 1)
		if err != nil {
			return err
		}
		partial = prev[0] != '\n'
	}
	data, err := b.readAt(f.file, start, f.info.Size()-start)
	if err != nil {
		return err
	}
	pos := 0
	if partial {
		n := bytes.IndexByte(data, '\n')
		if n < 0 || n+1 > maxRead {
			return budgetExceeded
		}
		pos = n + 1
	}
	for pos < len(data) {
		if b.ctx.Err() != nil {
			return unavailable
		}
		n := bytes.IndexByte(data[pos:], '\n')
		if n < 0 {
			if len(data)-pos >= maxRead {
				return budgetExceeded
			}
			break
		}
		if n+1 > maxRead {
			return budgetExceeded
		}
		var entry struct {
			Type      string          `json:"type"`
			SessionID string          `json:"sessionId"`
			CWD       string          `json:"cwd"`
			Effort    json.RawMessage `json:"effort"`
			Message   struct {
				Model json.RawMessage `json:"model"`
			} `json:"message"`
		}
		if json.Unmarshal(data[pos:pos+n], &entry) != nil {
			return malformed
		}
		if entry.SessionID != "" && entry.SessionID != o.config.Binding.SessionID || entry.CWD != "" && !samePath(entry.CWD, o.config.CWD) {
			return mismatch
		}
		if entry.Type == "assistant" {
			state.model, state.effort = selectedRuntimeJSON(entry.Message.Model, entry.Effort)
		}
		pos += n + 1
	}
	if err := f.validate(); err != nil {
		return err
	}
	state.offset = start + int64(pos)
	if pos > 0 {
		length := min(pos, anchorLimit)
		state.anchorLen = length
		state.anchorStart = state.offset - int64(length)
		state.anchorHash = sha256.Sum256(data[pos-length : pos])
	}
	o.runtimeVerified = true
	o.transcript = state
	return nil
}
