package localactivity

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
)

// CodexTurnEnded proves only the latest explicit same-turn terminal pair.
// It keeps no activity cache and never uses wall-clock time as end evidence.
func CodexTurnEnded(ctx context.Context, config Config, turnID string) bool {
	if turnID == "" || len(turnID) > 128 || config.Binding.Harness != "codex" || !sessionUUID.MatchString(config.Binding.SessionID) || !filepath.IsAbs(config.Home) || !filepath.IsAbs(config.CWD) {
		return false
	}
	o := New(config)
	process, err := o.config.Probe(ctx, config.PID)
	if err != nil || process.Key == "" || ctx.Err() != nil {
		return false
	}
	path, err := o.locateRollout(ctx)
	if err != nil {
		return false
	}
	f, err := o.openEvidence(path)
	if err != nil {
		return false
	}
	defer f.close()
	budget := &readBudget{ctx: ctx, left: maxRead}
	meta, err := budget.readSessionMeta(f.file, f.info.Size())
	if err != nil {
		return false
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
	if json.Unmarshal(meta, &envelope) != nil || envelope.Type != "session_meta" || envelope.Payload.ID != config.Binding.SessionID || (envelope.Payload.SessionID != "" && envelope.Payload.SessionID != config.Binding.SessionID) || !samePath(envelope.Payload.CWD, config.CWD) || (envelope.Payload.Host != "" && !strings.EqualFold(envelope.Payload.Host, o.config.Host)) {
		return false
	}
	length := min(f.info.Size(), budget.left)
	start := f.info.Size() - length
	tail, err := budget.readAt(f.file, start, length)
	if err != nil || len(tail) == 0 || tail[len(tail)-1] != '\n' {
		return false
	}
	if start > 0 {
		cut := bytes.IndexByte(tail, '\n')
		if cut < 0 {
			return false
		}
		tail = tail[cut+1:]
	}
	latest := ""
	ended := false
	for _, line := range bytes.Split(tail, []byte{'\n'}) {
		if ctx.Err() != nil {
			return false
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record struct {
			Type    string `json:"type"`
			Payload struct {
				Type   string `json:"type"`
				TurnID string `json:"turn_id"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &record) != nil {
			return false
		}
		if record.Type != "event_msg" {
			continue
		}
		switch record.Payload.Type {
		case "task_started":
			if record.Payload.TurnID == "" {
				return false
			}
			latest = record.Payload.TurnID
			ended = false
		case "task_complete", "turn_aborted":
			if record.Payload.TurnID == "" {
				return false
			}
			if latest == turnID && record.Payload.TurnID == latest {
				ended = true
			}
		}
	}
	after, err := o.config.Probe(ctx, config.PID)
	return latest == turnID && ended && err == nil && after == process && ctx.Err() == nil && f.validate() == nil
}
