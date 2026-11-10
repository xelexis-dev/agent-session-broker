package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const AdminSender = "broker-admin"
const MaxMessageBytes = 12000

// IsAdminSender reserves every case variant, including names saved before upgrade.
func IsAdminSender(name string) bool { return strings.EqualFold(name, AdminSender) }

// ValidateAnnouncement checks routing fields and reserves a full millisecond timestamp.
// The actual acceptance time is stamped by the broker when it enqueues.
func ValidateAnnouncement(body, recipient, registrationID string) error {
	data, err := json.Marshal(Message{From: AdminSender, To: recipient, Body: body, RegistrationID: registrationID, SentAt: time.Unix(0, 123*int64(time.Millisecond)).UTC()})
	if err != nil || strings.TrimSpace(body) == "" || len(data) > MaxMessageBytes {
		return errors.New("announcement requires a body and an envelope of at most 12000 bytes")
	}
	return nil
}
