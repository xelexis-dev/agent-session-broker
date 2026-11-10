package protocol

import (
	"encoding/json"
	"testing"
	"time"
)

func TestOldMessageWithoutSentAtDecodes(t *testing.T) {
	raw := []byte(`{"from":"a","to":"b","body":"old"}`)
	var message Message
	var delivered DeliveredMessage
	if err := json.Unmarshal(raw, &message); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &delivered); err != nil {
		t.Fatal(err)
	}
	if !message.SentAt.IsZero() || !delivered.SentAt.IsZero() || message.Body != "old" || delivered.Body != "old" {
		t.Fatal("old envelope changed")
	}
	for _, value := range []any{message, delivered} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		json.Unmarshal(data, &fields)
		if _, ok := fields["sentAt"]; ok {
			t.Fatal("absent timestamp must stay omitted")
		}
	}
}

func TestProjectionPreservesQueuedBatchForCapableReceiver(t *testing.T) {
	stamp := time.Date(2026, 10, 18, 1, 2, 3, 456000000, time.UTC)
	queued := Message{Batch: &MessageBatch{Messages: []DeliveredMessage{{From: "a", To: "b", Body: "one", SentAt: stamp}, {From: "c", To: "b", Body: "two", SentAt: stamp.Add(time.Second)}}}}
	old := ProjectMessage(queued, false)
	if !old.Batch.Messages[0].SentAt.IsZero() || !old.Batch.Messages[1].SentAt.IsZero() {
		t.Fatal("batch projected timestamps to old receiver")
	}
	modern := ProjectMessage(queued, true)
	if !modern.Batch.Messages[0].SentAt.Equal(stamp) || !modern.Batch.Messages[1].SentAt.Equal(stamp.Add(time.Second)) {
		t.Fatal("projection mutated queued timestamps")
	}
}
