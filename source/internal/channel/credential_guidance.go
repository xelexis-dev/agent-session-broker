package channel

// A 401 does not distinguish revocation, expiry and an invalid credential.
// Never use an upstream error or response body to explain authorization.
func credentialGuidance(device bool) string {
	if device {
		return "credential-rejected: The broker no longer accepts this session credential; it may be revoked, detached or expired. Call attach_session to request a new approval code, then ask the user to approve it. If the project admission token is no longer valid, ask the user to replace it in the MCP configuration first. An existing registration may remain until server retention cleanup."
	}
	return "credential-rejected: The broker no longer accepts the configured session credential; it may be revoked, expired or invalid. Ask the user to replace it in this MCP client's configuration with a valid session credential issued for this same conversation, or configure a project admission token to request approval. Then reconnect the MCP client using the same local state directory and call attach_session to resume or authorize this session."
}

func upstreamCredentialGuidance(u Upstream, device bool) string {
	if adviser, ok := u.(interface{ rejectionGuidance() string }); ok {
		if message := adviser.rejectionGuidance(); message != "" {
			return message
		}
	}
	return credentialGuidance(device)
}
