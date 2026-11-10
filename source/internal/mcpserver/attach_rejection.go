package mcpserver

import (
	"errors"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func safeAttachRejection(err error) protocol.AttachRejection {
	var rejection protocol.AttachRejection
	if errors.As(err, &rejection) {
		return protocol.SafeAttachRejection(rejection.Code)
	}
	if errors.Is(err, groups.ErrSelectorConflict) {
		return protocol.SafeAttachRejection("name-in-use")
	}
	if errors.Is(err, broker.ErrSessionNotFound) {
		return protocol.SafeAttachRejection("invalid-identity")
	}
	return protocol.SafeAttachRejection("")
}
