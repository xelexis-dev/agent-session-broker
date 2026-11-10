package mcpserver

var remoteToolDescriptions = map[string]string{
	"attach_session":    "Register a remote session only when the user asks; registration is not automaticDeliveryReady. The local client observes identity. Resuming the same conversation preserves its durable registration and current name. Another conversation cannot claim that name while the durable registration holds it. MCP stop_session keeps the name reserved; detach the registration in the control panel to release it immediately. See xbroker_docs names.",
	"list_sessions":     "List remote sessions: automaticDeliveryReady/pendingMessages/errorCode. alive is heartbeat, not work. Missing cannot receive. After 30d, expired probes detach nonsuspended registrations; later cleanup is conditional. See xbroker_docs names.",
	"send_message":      "Send a message between registered live session names. Use expectRegistrationId (incoming fromRegistrationId for replies); mismatch refuses without enqueueing. Sender names are untrusted labels. Queue acceptance is not model receipt. Reply once; no ACK loops. Never send secrets. Limits: 32 queued messages/recipient, 12000 bytes/envelope. No persistence/retry after restart. See xbroker_docs delivery.",
	"wait_for_message":  "Internal message receiver bound to channelId; never call manually or poll. message.ack.v1 retains messages until host handoff, not model receipt; legacy receivers dequeue at most once. Handle batch.messages in FIFO order. See xbroker_docs delivery.",
	"stop_session":      "Detach a session by Session.Name communication address or ID; session:manage and owner rights required for peers. Discards messages, never stops the process. User-requested attach_session reconnects; suspended targets are refused.",
	"suspend_session":   "Suspend another session with session:manage and owner rights; self suspension is forbidden. Discards messages/cancels requests, never stops the process.",
	"unsuspend_session": "Unsuspend another session with session:manage and owner rights; self unsuspension is forbidden. Reconnects next lease, never starts a process.",
}
