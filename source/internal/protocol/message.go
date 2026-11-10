package protocol

import (
	"slices"
	"time"
)

const MessageAckCapability = "message.ack.v1"

func SupportsMessageAck(capabilities []string) bool {
	return slices.Contains(capabilities, MessageAckCapability)
}

type IngressRequest struct {
	Recipient      string `json:"recipient" jsonschema:"Session.Name or session ID whose queue this receiver owns"`
	ChannelID      string `json:"channelId,omitempty"`
	RegistrationID string `json:"registrationId,omitempty"`
	Epoch          string `json:"epoch,omitempty"`
	AckThrough     uint64 `json:"ackThrough,omitempty"`
	Cursor         uint64 `json:"cursor,omitempty"`
}

const MessageSentAtCapability = "message.sentAt"

func SupportsMessageSentAt(capabilities []string) bool {
	return slices.Contains(capabilities, MessageSentAtCapability)
}

// ProjectMessage never changes the accepted queue item or its batch slice.
func ProjectMessage(message Message, sentAt bool) Message {
	if sentAt {
		return message
	}
	message.SentAt = time.Time{}
	if message.Batch != nil {
		entries := slices.Clone(message.Batch.Messages)
		for i := range entries {
			entries[i].SentAt = time.Time{}
		}
		message.Batch = &MessageBatch{Messages: entries}
	}
	return message
}
