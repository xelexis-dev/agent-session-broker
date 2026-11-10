package claude

import (
	"context"
	"errors"
	"strings"

	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

var errInvalidParentIdentity = errors.New("invalid Claude parent identity")

// InspectParent is for an MCP client's exact ancestor PID only. Unlike wake's
// interactive inspection, a background row's short ID is never an identity.
func (a *Adapter) InspectParent(ctx context.Context, pid int) (broker.ClaudeIdentity, error) {
	if pid <= 0 {
		return broker.ClaudeIdentity{}, errInvalidParentIdentity
	}
	rows, err := a.rawSessions(ctx)
	if err != nil {
		return broker.ClaudeIdentity{}, sanitizeError("inspect parent Claude failed", err)
	}
	var parent *rawSession
	for i := range rows {
		if rows[i].PID != pid {
			continue
		}
		if parent != nil {
			return broker.ClaudeIdentity{}, errInvalidParentIdentity
		}
		parent = &rows[i]
	}
	if parent == nil {
		return broker.ClaudeIdentity{}, ErrSessionNotFound
	}
	if (parent.Kind != "interactive" && parent.Kind != "background") || !validCanonicalUUID(parent.SessionID) || parent.CWD == "" || strings.ContainsRune(parent.CWD, 0) {
		return broker.ClaudeIdentity{}, errInvalidParentIdentity
	}
	// Dialog and permission waits also belong to a live background parent.
	// Keep the shared roster/wake status interpretation unchanged.
	row := *parent
	if row.Kind == "background" {
		// Background state describes the previous turn (including "done"),
		// not whether this live MCP parent can receive another message.
		row.State = ""
	}
	row.Kind = "interactive"
	status := a.statusWithProcessLiveness(pid, row.normalizedStatus())
	if status != broker.StatusIdle && status != broker.StatusWorking {
		return broker.ClaudeIdentity{}, ErrSessionNotFound
	}
	return broker.ClaudeIdentity{PID: pid, ID: parent.SessionID, ConversationID: parent.SessionID, Name: parent.Name, CWD: parent.CWD, Status: status}, nil
}
