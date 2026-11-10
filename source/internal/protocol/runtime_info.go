package protocol

import "time"

const RuntimeInfoVersion = 1

// RuntimeInfo is display-only runtime metadata, deliberately absent from Session.
type RuntimeInfo struct {
	Model      string    `json:"model,omitempty"`
	Effort     string    `json:"effort,omitempty"`
	Sequence   uint64    `json:"sequence"`
	Generation uint64    `json:"generation"`
	ObservedAt time.Time `json:"observedAt"`
}
type RuntimeInfoSnapshot struct {
	RuntimeInfo
	ReceivedAt time.Time `json:"receivedAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// ValidRuntimeLabel accepts only short, printable identifier metadata.
func ValidRuntimeLabel(s string, limit int) bool {
	if len(s) > limit {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
