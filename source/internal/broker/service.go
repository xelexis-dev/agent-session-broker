package broker

import (
	"errors"

	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// ErrSessionIdentityUnauthorized is fixed and safe to expose through guidance.
var ErrSessionIdentityUnauthorized = errors.New("session token does not authorize this identity")

var ErrSessionNotFound = errors.New("session not found")

type ClaudeIdentity = protocol.ClaudeIdentity
