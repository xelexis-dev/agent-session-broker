package protocol

import "time"

type HandoverFrom struct {
	Revision uint64    `json:"revision"`
	At       time.Time `json:"at"`
}
type HandoverAudit struct {
	Previous       string    `json:"previous"`
	Current        string    `json:"current"`
	RegistrationID string    `json:"registrationId"`
	Name           string    `json:"name"`
	At             time.Time `json:"at"`
}
type HandoverRefusal struct {
	Reason          string
	HolderChallenge *HolderChallenge
}

func (e HandoverRefusal) Error() string { return "handover-refused: " + SafeHandoverReason(e.Reason) }
func SafeHandoverReason(reason string) string {
	switch reason {
	case "registration-in-use", "approval-required", "still-receiving", "secret-mismatch", "client-mismatch", "owner-mismatch", "suspended", "not-found", "unsupported-mode", "identity-conflict", "revision-mismatch":
		return reason
	default:
		return "refused"
	}
}
