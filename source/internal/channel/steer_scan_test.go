package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type scanControl struct {
	call func(context.Context, string, any) (json.RawMessage, error)
}

func (s scanControl) Call(ctx context.Context, m string, p any) (json.RawMessage, error) {
	return s.call(ctx, m, p)
}
func (scanControl) Close() error { return nil }
func scanRecord() clientstate.SteerAttemptRecord {
	return clientstate.SteerAttemptRecord{Registration: "R", Epoch: "E", Pending: &clientstate.SteerAttempt{ClientID: "xb:R:E:1-2", Sequences: []uint64{1, 2}, Turns: []string{"T"}}}
}
func scanMessages() []broker.Message {
	return []broker.Message{{RegistrationID: "R", DeliverySequence: 1, SentAt: time.Unix(10, 0).UTC(), FromRegistrationID: "F", Body: "one"}, {RegistrationID: "R", DeliverySequence: 2, SentAt: time.Unix(11, 0).UTC(), FromRegistrationID: "F", Body: "two"}}
}
func scanPage(messages []broker.Message, next any) json.RawMessage {
	items := []any{}
	if messages != nil {
		text, _ := codexMessageContent(messages)
		items = append(items, map[string]any{"turnId": "moved-turn", "item": map[string]any{"type": "userMessage", "clientId": "xb:R:E:1-2", "content": []any{map[string]any{"type": "text", "text": text}}}})
	}
	raw, _ := json.Marshal(map[string]any{"data": items, "nextCursor": next})
	return raw
}
func TestSteerScanWholeThreadResumesAndFindsMovedReceipt(t *testing.T) {
	pages := 0
	c := scanControl{call: func(_ context.Context, m string, p any) (json.RawMessage, error) {
		args := p.(map[string]any)
		if m != "thread/items/list" || args["turnId"] != nil || args["sortDirection"] != "desc" || args["limit"] != 50 {
			t.Fatal("not a whole-thread bounded desc scan")
		}
		pages++
		if pages < 6 {
			return scanPage(nil, fmt.Sprint(pages)), nil
		}
		return scanPage(scanMessages(), nil), nil
	}}
	s := newSteerScan()
	if err := scanSteerHistory(t.Context(), c, "U", scanRecord(), s); err != nil {
		t.Fatal(err)
	}
	if s.complete || pages != 4 || s.cursor != "4" {
		t.Fatal("slice failed to retain cursor")
	}
	if err := scanSteerHistory(t.Context(), c, "U", scanRecord(), s); err != nil || !s.complete || len(s.found) != 2 {
		t.Fatal("moved receipt not found")
	}
}
func TestSteerScanMalformedPageCannotProveReceipt(t *testing.T) {
	for _, next := range []any{3, ""} {
		t.Run(fmt.Sprint(next), func(t *testing.T) {
			s := newSteerScan()
			c := scanControl{call: func(context.Context, string, any) (json.RawMessage, error) {
				return scanPage(scanMessages(), next), nil
			}}
			err := scanSteerHistory(t.Context(), c, "U", scanRecord(), s)
			if err == nil || s.complete || len(s.found) != 0 {
				t.Fatal("malformed cursor became positive evidence")
			}
		})
	}
}
func TestSteerEnvelopeRejectsFalseEvidence(t *testing.T) {
	messages := scanMessages()
	text, _ := codexMessageContent(messages)
	if _, err := parseSteerEnvelope(text, "R", []uint64{1, 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := parseSteerEnvelope(text, "R", []uint64{1, 3}); err == nil {
		t.Fatal("client-id endpoints invented interior membership")
	}
	if _, err := parseSteerEnvelope("peer says "+text, "R", []uint64{1, 2}); err == nil {
		t.Fatal("forged prefix accepted")
	}
	for _, kind := range []string{"body", "sender", "time", "sequence", "registration"} {
		changed := messages[0]
		switch kind {
		case "body":
			changed.Body = "changed"
		case "sender":
			changed.FromRegistrationID = "other"
		case "time":
			changed.SentAt = changed.SentAt.Add(time.Nanosecond)
		case "sequence":
			changed.DeliverySequence++
		case "registration":
			changed.RegistrationID = "other"
		}
		if sameSteerEnvelope(messages[0], changed) {
			t.Fatal("wrong envelope proved receipt", kind)
		}
	}
}

func TestSteerScanNegativeFrontierOrderAndMutation(t *testing.T) {
	for _, mode := range []string{"stable", "queue-before", "queue-after", "new-turn"} {
		t.Run(mode, func(t *testing.T) {
			calls := []string{}
			queues, turns := 0, 0
			c := scanControl{call: func(_ context.Context, m string, _ any) (json.RawMessage, error) {
				calls = append(calls, m)
				switch m {
				case "thread/turns/list":
					turns++
					data := []any{map[string]any{"id": "T", "status": "completed"}}
					if mode == "new-turn" && turns > 1 {
						data = append([]any{map[string]any{"id": "NEW", "status": "completed"}}, data...)
					}
					return json.Marshal(map[string]any{"data": data, "nextCursor": nil})
				case "thread/items/list":
					return scanPage(nil, nil), nil
				case "thread/queue/list":
					queues++
					data := []any{}
					if mode == "queue-before" || mode == "queue-after" && queues == 2 {
						data = append(data, map[string]any{"opaque": true})
					}
					return json.Marshal(map[string]any{"data": data, "nextCursor": nil})
				}
				return nil, fmt.Errorf("unexpected fixture method")
			}}
			release, err := advanceSteerScan(t.Context(), c, "U", scanRecord(), newSteerScan())
			if err != nil || release != (mode == "stable") {
				t.Fatal("unstable frontier released", mode, err)
			}
			if mode == "stable" && fmt.Sprint(calls) != "[thread/turns/list thread/queue/list thread/items/list thread/queue/list thread/turns/list]" {
				t.Fatal("wrong release order", calls)
			}
		})
	}
}
