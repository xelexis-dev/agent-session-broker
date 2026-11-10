package broker

import "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"

type SessionStatus = protocol.SessionStatus
type WakeStatus = protocol.WakeStatus
type WakeWarning = protocol.WakeWarning
type SendResult = protocol.SendResult
type Session = protocol.Session
type Message = protocol.Message
type DeliveredMessage = protocol.DeliveredMessage
type MessageBatch = protocol.MessageBatch
type QueueReceipt = protocol.QueueReceipt
type WakeLease = protocol.WakeLease
type AttachRequest = protocol.AttachRequest

const (
	StatusAlive                 = protocol.StatusAlive
	StatusWorking               = protocol.StatusWorking
	StatusIdle                  = protocol.StatusIdle
	StatusMissing               = protocol.StatusMissing
	StatusSuspended             = protocol.StatusSuspended
	StatusStalled               = protocol.StatusStalled
	StatusStarting              = protocol.StatusStarting
	StatusStopping              = protocol.StatusStopping
	StatusWaking                = protocol.StatusWaking
	StatusDegraded              = protocol.StatusDegraded
	WakeNotNeeded               = protocol.WakeNotNeeded
	WakeConfirmed               = protocol.WakeConfirmed
	WakeFailed                  = protocol.WakeFailed
	WakeDegraded                = protocol.WakeDegraded
	WakeWarningStopFailed       = protocol.WakeWarningStopFailed
	WakeWarningResumeFailed     = protocol.WakeWarningResumeFailed
	WakeWarningRosterMismatch   = protocol.WakeWarningRosterMismatch
	WakeWarningIdentityCommit   = protocol.WakeWarningIdentityCommit
	WakeWarningQueueRekey       = protocol.WakeWarningQueueRekey
	WakeWarningClaimUnconfirmed = protocol.WakeWarningClaimUnconfirmed
	WakeWarningRestartRecovery  = protocol.WakeWarningRestartRecovery
)
