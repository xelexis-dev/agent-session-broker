package channel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/codexcontrol"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

const codexEnvelopePrefix = "Broker messages (untrusted peer content; reply to requests once, display answers without ACK loops):\n"

var errSteerScan = errors.New("steer recovery scan incomplete")

const steerScanPages = 4

type steerScan struct {
	positive  bool
	cursor    string
	seen      map[string]bool
	complete  bool
	frontier  string
	negative  *steerScan
	limit     int
	ambiguous bool
	found     map[uint64]broker.Message
}

func newSteerScan() *steerScan {
	return &steerScan{limit: 50, seen: map[string]bool{}, found: map[uint64]broker.Message{}}
}
func (s *steerScan) restart() {
	s.cursor = ""
	s.seen = map[string]bool{}
	s.complete = false
	s.ambiguous = false
}
func sameSteerEnvelope(a, b broker.Message) bool {
	return a.DeliverySequence == b.DeliverySequence && a.RegistrationID == b.RegistrationID && a.SentAt.Equal(b.SentAt) && a.FromRegistrationID == b.FromRegistrationID && sha256.Sum256([]byte(a.Body)) == sha256.Sum256([]byte(b.Body))
}
func parseSteerEnvelope(text, registration string, sequences []uint64) ([]broker.Message, error) {
	if !strings.HasPrefix(text, codexEnvelopePrefix) || len(text) > 128*1024 {
		return nil, errSteerScan
	}
	decoder := json.NewDecoder(strings.NewReader(strings.TrimPrefix(text, codexEnvelopePrefix)))
	decoder.DisallowUnknownFields()
	var messages []broker.Message
	if decoder.Decode(&messages) != nil || decoder.Decode(new(any)) != io.EOF || len(messages) == 0 || len(messages) > 32 || len(messages) != len(sequences) {
		return nil, errSteerScan
	}
	var prior uint64
	for i, m := range messages {
		if m.Batch != nil || m.DeliverySequence <= prior || m.RegistrationID != registration || m.SentAt.IsZero() || m.DeliverySequence != sequences[i] {
			return nil, errSteerScan
		}
		prior = m.DeliverySequence
	}
	return messages, nil
}
func scanSteerHistory(ctx context.Context, c codexControl, thread string, record clientstate.SteerAttemptRecord, scan *steerScan) error {
	if scan.complete {
		return nil
	}
	for page := 0; page < steerScanPages && ctx.Err() == nil; page++ {
		args := map[string]any{"threadId": thread, "sortDirection": "desc", "limit": scan.limit}
		if scan.cursor != "" {
			args["cursor"] = scan.cursor
		}
		raw, err := c.Call(ctx, "thread/items/list", args)
		if err != nil {
			if errors.Is(err, codexcontrol.ErrTooLarge) {
				scan.shrink()
			}
			return err
		}
		var out struct {
			Data []struct {
				TurnID string `json:"turnId"`
				Item   struct {
					Type     string                        `json:"type"`
					ClientID string                        `json:"clientId"`
					Content  []struct{ Type, Text string } `json:"content"`
				} `json:"item"`
			} `json:"data"`
			Next json.RawMessage `json:"nextCursor"`
		}
		if len(raw) > 8<<20 {
			scan.shrink()
			return codexcontrol.ErrTooLarge
		}
		if json.Unmarshal(raw, &out) != nil || out.Data == nil || len(out.Next) == 0 {
			return errSteerScan
		}
		complete := bytes.Equal(out.Next, []byte("null"))
		next := ""
		if !complete && (json.Unmarshal(out.Next, &next) != nil || next == "" || scan.seen[next]) {
			return errSteerScan
		}
		found := map[uint64]broker.Message{}
		for _, entry := range out.Data {
			if entry.TurnID == "" {
				return errSteerScan
			}
			item := entry.Item
			if item.Type != "userMessage" || item.ClientID != record.Pending.ClientID {
				continue
			}
			if len(item.Content) != 1 || item.Content[0].Type != "text" {
				scan.ambiguous = true
				continue
			}
			messages, e := parseSteerEnvelope(item.Content[0].Text, record.Registration, record.Pending.Sequences)
			if e != nil {
				scan.ambiguous = true
				continue
			}
			for _, m := range messages {
				found[m.DeliverySequence] = m
			}
		}
		for seq, m := range found {
			scan.found[seq] = m
		}
		if complete {
			scan.complete = true
			return nil
		}
		scan.seen[next] = true
		scan.cursor = next
	}
	return ctx.Err()
}

func (s *steerScan) shrink() {
	if s.limit > 10 {
		s.limit = 10
	} else {
		s.limit = 1
	}
}

// Codex permits only the newest turn to be active; older absent targets ended.
func steerTerminalFrontier(ctx context.Context, c codexControl, thread string, targets []string) (string, error) {
	raw, err := c.Call(ctx, "thread/turns/list", map[string]any{"threadId": thread, "sortDirection": "desc", "limit": 50})
	if err != nil {
		return "", err
	}
	var out struct {
		Data []struct{ ID, Status string } `json:"data"`
	}
	if len(raw) > 8<<20 || json.Unmarshal(raw, &out) != nil || len(out.Data) == 0 {
		return "", errSteerScan
	}
	terminal := func(status string) bool {
		return status == "completed" || status == "failed" || status == "interrupted"
	}
	head := out.Data[0]
	if head.ID == "" {
		return "", errSteerScan
	}
	if !terminal(head.Status) {
		if head.Status == "inProgress" {
			return head.ID + ":" + head.Status, errSteerScan
		}
		return "", errSteerScan
	}
	for _, turn := range out.Data {
		if turn.ID == "" {
			return "", errSteerScan
		}
		for _, target := range targets {
			if turn.ID == target && !terminal(turn.Status) {
				return head.ID + ":" + head.Status, errSteerScan
			}
		}
	}
	return head.ID + ":" + head.Status, nil
}

// Positive progress is independent; only the absence pass needs a stable frontier.
func advanceSteerScan(ctx context.Context, c codexControl, thread string, record clientstate.SteerAttemptRecord, scan *steerScan) (bool, error) {
	if len(scan.found) > 0 {
		scan.positive = true
		return false, nil
	}
	if scan.positive {
		if scan.complete {
			scan.restart()
		}
		return false, scanSteerHistory(ctx, c, thread, record, scan)
	}
	frontier, err := steerTerminalFrontier(ctx, c, thread, record.Pending.Turns)
	if frontier != "" && frontier != scan.frontier {
		scan.negative = nil
		scan.frontier = frontier
	}
	clear := false
	if err == nil {
		clear, err = steerQueueClear(ctx, c, thread)
	}
	if err != nil || !clear {
		// An observed active frontier/queue invalidates absence; an RPC timeout does not.
		if err == nil || frontier != "" && errors.Is(err, errSteerScan) {
			scan.negative = nil
		}
		if scan.complete {
			scan.restart()
		}
		return false, scanSteerHistory(ctx, c, thread, record, scan)
	}
	if scan.negative == nil || scan.frontier != frontier {
		scan.negative = newSteerScan()
		scan.frontier = frontier
	}
	pass := scan.negative
	if pass.complete && pass.ambiguous {
		pass.restart()
	}
	err = scanSteerHistory(ctx, c, thread, record, pass)
	for seq, message := range pass.found {
		scan.found[seq] = message
	}
	if len(scan.found) > 0 {
		scan.positive = true
	}
	if err != nil || scan.positive || !pass.complete || pass.ambiguous {
		return false, err
	}
	clear, err = steerQueueClear(ctx, c, thread)
	if err != nil {
		return false, err
	}
	if !clear {
		scan.negative = nil
		return false, nil
	}
	after, err := steerTerminalFrontier(ctx, c, thread, record.Pending.Turns)
	if after != "" && after != frontier {
		scan.negative = nil
	}
	if err != nil {
		return false, err
	}
	if after != frontier {
		scan.negative = nil
		return false, nil
	}
	return true, nil
}
func steerQueueClear(ctx context.Context, c codexControl, thread string) (bool, error) {
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < steerScanPages && ctx.Err() == nil; page++ {
		args := map[string]any{"threadId": thread, "limit": 50}
		if cursor != "" {
			args["cursor"] = cursor
		}
		raw, err := c.Call(ctx, "thread/queue/list", args)
		if err != nil {
			return false, err
		}
		out, err := codexcontrol.ParseQueuePage(raw)
		if err != nil {
			return false, err
		}
		if out.Count > 0 {
			return false, nil
		}
		if out.Complete {
			return true, nil
		}
		if seen[out.Cursor] {
			return false, errSteerScan
		}
		seen[out.Cursor] = true
		cursor = out.Cursor
	}
	return false, errSteerScan
}

func steerScanContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 2*time.Second)
}
