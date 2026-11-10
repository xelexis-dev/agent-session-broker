package protocol

import "time"

// ActivityVersion gates optional activity input. Activity is display evidence,
// never liveness, authorization, delivery readiness or registration policy.
const ActivityVersion = 1

type ActivityState string

const (
	ActivityWorking ActivityState = "working"
	ActivityIdle    ActivityState = "idle"
	ActivityUnknown ActivityState = "unknown"
)

// Activity is the client's bounded observation. Generation, when supplied,
// must match the server-issued current lease; it is not an authorization token.
// Source and Reason contain only fixed codes, never paths or raw errors.
type Activity struct {
	State      ActivityState `json:"state"`
	Source     string        `json:"source,omitempty"`
	Sequence   uint64        `json:"sequence"`
	Generation uint64        `json:"generation,omitempty"`
	ObservedAt time.Time     `json:"observedAt,omitzero"`
	ChangedAt  time.Time     `json:"changedAt,omitzero"`
	Reason     string        `json:"reason,omitempty"`
}

// ActivitySnapshot is a copy of volatile server state. Only server receipt
// time determines freshness; client timestamps cannot extend it.
type ActivitySnapshot struct {
	Activity
	ReceivedAt time.Time `json:"receivedAt,omitzero"`
	ExpiresAt  time.Time `json:"expiresAt,omitzero"`
}
