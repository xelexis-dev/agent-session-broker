package channel

import (
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// Broker text is untrusted; only known reasons and a validated ID enter guidance.
func safeRenameRejection(result *mcp.CallToolResult) error {
	const generic = "broker rename_session rejected the request; server gave no reason"
	var data []byte
	var err error
	if result.StructuredContent != nil {
		data, err = json.Marshal(result.StructuredContent)
	} else if len(result.Content) > 0 {
		if text, ok := result.Content[0].(*mcp.TextContent); ok && len(text.Text) <= 4096 {
			data = []byte(text.Text)
		}
	}
	var reason protocol.RenameRejection
	if err != nil || len(data) > 4096 || json.Unmarshal(data, &reason) != nil || reason.Code != "nickname-in-use" {
		return errors.New(generic)
	}
	var message string
	switch reason.Holder {
	case "live-session":
		message = "a live session holds this nickname. Choose another nickname. If the session has been replaced, ask an authorized manager to call retire_session with its exact registrationId."
	case "inactive-registration":
		message = "an inactive registration holds this nickname. Ask an authorized manager to call retire_session with its exact registrationId."
	case "detached-registration":
		message = "a detached registration holds this nickname. Ask an authorized manager to call retire_session with its exact registrationId."
	case "orphaned-credential":
		return errors.New("nickname-in-use: a previously connected orphan credential reserves this nickname. Ask an authorized manager to call retire_session with name set to the requested nickname.")
	case "unused-credential":
		return errors.New("nickname-in-use: an unused credential reserves this nickname. Use the administration panel to release it or wait for expiry; retire_session cannot reclaim unused credentials.")
	default:
		return errors.New(generic)
	}
	if protocol.ValidRegistrationID(reason.HolderRegistrationID) {
		message += " Use registrationId=" + reason.HolderRegistrationID + "."
	} else {
		message += " Use list_sessions to find the exact registrationId."
	}
	return errors.New("nickname-in-use: " + message)
}
