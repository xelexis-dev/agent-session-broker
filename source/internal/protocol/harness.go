package protocol

const ClaudeWakePrompt = "You are resuming an existing broker-managed session. Use the already configured project broker. Call wait_for_message exactly once with your established Session.Name. Handle the returned from/to/body envelope, or every envelope in batch.messages in order when a batch is present. Do not retry or invent a delivery."

type ClaudeIdentity struct {
	PID            int
	ID             string
	ConversationID string
	Name           string
	CWD            string
	Status         SessionStatus
}

type ClaudeWakeRequest struct {
	BrokerURL string
	Name      string
	Old       ClaudeIdentity
}

type ClaudeCleanupTarget struct {
	AllowCompleted bool // Only current memberships with a persisted conversation ID.
	ID             string
	ConversationID string
	Name           string
}

type ClaudeWakeResult struct {
	OldStopped bool
	OldActive  bool
	Metered    bool
	New        ClaudeIdentity
}
