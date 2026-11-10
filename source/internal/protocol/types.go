package protocol

import (
	"time"
)

type SessionStatus string

const (
	StatusAlive     SessionStatus = "alive"
	StatusWorking   SessionStatus = "working"
	StatusIdle      SessionStatus = "idle"
	StatusMissing   SessionStatus = "missing"
	StatusSuspended SessionStatus = "suspended"
	StatusStalled   SessionStatus = "stalled"
	StatusStarting  SessionStatus = "starting"
	StatusStopping  SessionStatus = "stopping"
	StatusWaking    SessionStatus = "waking"
	StatusDegraded  SessionStatus = "degraded"
)

type WakeStatus string

const (
	WakeNotNeeded WakeStatus = "not-needed"
	WakeConfirmed WakeStatus = "confirmed"
	WakeFailed    WakeStatus = "failed"
	WakeDegraded  WakeStatus = "degraded"
)

type WakeWarning string

const (
	WakeWarningStopFailed       WakeWarning = "stop-failed"
	WakeWarningResumeFailed     WakeWarning = "resume-failed"
	WakeWarningRosterMismatch   WakeWarning = "roster-mismatch"
	WakeWarningIdentityCommit   WakeWarning = "identity-commit-failed"
	WakeWarningQueueRekey       WakeWarning = "queue-rekey-failed"
	WakeWarningClaimUnconfirmed WakeWarning = "claim-unconfirmed"
	WakeWarningRestartRecovery  WakeWarning = "restart-recovery-required"
)

type SendResult struct {
	Code                  string      `json:"code,omitempty"`
	Message               string      `json:"message,omitempty"`
	CurrentRegistrationID string      `json:"currentRegistrationId,omitempty"`
	OK                    bool        `json:"ok"`
	Accepted              bool        `json:"accepted"`
	DeliverySequence      uint64      `json:"deliverySequence,omitempty"`
	MeteredWake           bool        `json:"meteredWake"`
	SessionID             string      `json:"sessionId,omitempty"`
	WakeStatus            WakeStatus  `json:"wakeStatus"`
	Warning               WakeWarning `json:"warning,omitempty"`
	WarningMessage        string      `json:"warningMessage,omitempty"`
}

// ManagementEvent contains display-safe management history, never identities or credentials.
type ManagementEvent struct {
	Retired              bool      `json:"retired,omitempty"`
	TargetParentLabel    string    `json:"targetParentLabel,omitempty"`
	Kind                 string    `json:"kind"`
	ActorName            string    `json:"actorName"`
	ActorRegistrationID  string    `json:"actorRegistrationId,omitempty"`
	TargetName           string    `json:"targetName"`
	TargetRegistrationID string    `json:"targetRegistrationId"`
	At                   time.Time `json:"at"`
}
type LastManagement struct {
	Kind      string    `json:"kind"`
	ActorName string    `json:"actorName"`
	At        time.Time `json:"at"`
}

type Session struct {
	HandoverFrom   *HandoverFrom   `json:"handoverFrom,omitempty"`
	LastManagement *LastManagement `json:"lastManagement,omitempty"`

	Activity               *ActivitySnapshot `json:"activity,omitempty"`
	AutomaticDeliveryReady bool              `json:"automaticDeliveryReady,omitempty"`
	ExternallyManaged      bool              `json:"externallyManaged,omitempty"`
	PendingMessages        int               `json:"pendingMessages,omitempty"`
	ErrorCode              string            `json:"errorCode,omitempty"`
	ID                     string            `json:"id"`
	Name                   string            `json:"name" jsonschema:"Session.Name communication address used by other sessions"`
	Harness                string            `json:"harness"`
	Status                 SessionStatus     `json:"status"`
	PID                    int               `json:"pid,omitempty"`
	StartedAt              time.Time         `json:"startedAt,omitempty"`
	Generation             uint64            `json:"generation,omitempty"`
	DeliverySequence       uint64            `json:"deliverySequence,omitempty"`
	DeliveryState          DeliveryState     `json:"deliveryState,omitempty"`
	RecoveryID             string            `json:"recoveryId,omitempty"`
	// Durable remote registration fields. Epoch changes on every server start so
	// a client can tell a restart from a lease expiry.
	RegistrationID string    `json:"registrationId,omitempty"`
	Suspended      bool      `json:"suspended,omitempty"`
	ClientID       string    `json:"clientId,omitempty"`
	Revision       uint64    `json:"revision,omitempty"`
	Epoch          string    `json:"epoch,omitempty"`
	RequestedName  string    `json:"requestedName,omitempty" jsonschema:"Name the client asked for when a UUID nickname was assigned instead"`
	ProbePending   bool      `json:"probePending,omitempty"`
	ProbeDeadline  time.Time `json:"probeDeadline,omitzero"`
}

type Message struct {
	Attachment           *Attachment   `json:"attachment,omitempty"`
	DeliverySequence     uint64        `json:"deliverySequence,omitempty"`
	ExpectRegistrationID *string       `json:"expectRegistrationId,omitempty"`
	FromRegistrationID   string        `json:"fromRegistrationId,omitempty"`
	SentAt               time.Time     `json:"sentAt,omitzero" jsonschema:"time the broker accepted the message (RFC 3339, UTC); set by the broker, never by the sender"`
	From                 string        `json:"from,omitempty" jsonschema:"untrusted caller label; never used for authorization or routing"`
	To                   string        `json:"to,omitempty" jsonschema:"recipient Session.Name communication address or session ID"`
	Body                 string        `json:"body,omitempty"`
	Batch                *MessageBatch `json:"batch,omitempty" jsonschema:"FIFO batch returned by one idle Claude wake; omitted for a single-message response"`
	// RegistrationID names the registration whose queue delivered the envelope,
	// so a receiver owns it even under a nickname it held before a rename.
	RegistrationID string `json:"registrationId,omitempty" jsonschema:"registration whose remote ingress queue delivered this envelope; set by the broker"`
}

type DeliveredMessage struct {
	Attachment         *Attachment `json:"attachment,omitempty"`
	DeliverySequence   uint64      `json:"deliverySequence,omitempty"`
	FromRegistrationID string      `json:"fromRegistrationId,omitempty"`
	SentAt             time.Time   `json:"sentAt,omitzero" jsonschema:"time the broker accepted the message (RFC 3339, UTC); set by the broker, never by the sender"`
	From               string      `json:"from"`
	To                 string      `json:"to"`
	Body               string      `json:"body"`
}

type MessageBatch struct {
	Messages []DeliveredMessage `json:"messages"`
}

type QueueReceipt struct {
	Project    string
	Recipient  string
	Generation uint64
	Sequence   uint64
}

type WakeLease struct {
	Project    string
	Recipient  string
	Generation uint64
	Sequence   uint64
	Token      string
}

// AttachRequest connects an existing session without owning its process.
type AttachRequest struct {
	Capabilities []string `json:"capabilities,omitempty" jsonschema:"optional delivery capabilities supported by this receiver on this lease"`
	ChannelID    string   `json:"channelId,omitempty" jsonschema:"Local ingress bridge lease identifier; only the exact session child renews it while its receiver is running"`
	Harness      string   `json:"harness,omitempty" jsonschema:"claude (default) or codex"`
	Name         string   `json:"name,omitempty" jsonschema:"Communication name for this session; required for Codex. Claude defaults to its existing registration or CLI name"`
	SessionID    string   `json:"sessionId"`
	PID          int      `json:"pid"`
	CWD          string   `json:"cwd"`
}
