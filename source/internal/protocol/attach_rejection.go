package protocol

// AttachRejection contains only an allowlisted reason and a fixed user guide.
// Consumers must rebuild it from Code rather than trusting a remote Message.
type AttachRejection struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func SafeAttachRejection(code string) AttachRejection {
	var message string
	switch code {
	case "name-in-use":
		message = "Choose another communication name or ask an administrator to detach the old session."
	case "lease-conflict":
		message = "Reconnect this session's MCP client to obtain a fresh ingress lease."
	case "invalid-identity":
		message = "Check that this MCP client is connected to the intended live session, then register again."
	case "identity-conflict":
		message = "Detach the existing registration before changing this session's communication identity."
	case "inspection-unavailable":
		message = "Session inspection is unavailable. Check the local agent installation and connection."
	case "unsupported-harness":
		message = "Use a supported Claude or Codex session to register."
	case "legacy-disabled":
		message = "Register through the local MCP client for this token-protected project."
	case "capacity":
		message = "Ask an administrator to detach old sessions before registering another session."
	default:
		code = "attach-rejected"
		message = "Registration was rejected. Check the session identity and connection, then try again."
	}
	return AttachRejection{Code: code, Message: message}
}

func (e AttachRejection) Error() string {
	safe := SafeAttachRejection(e.Code)
	return safe.Code + ": " + safe.Message
}
