package channel

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

const localClientLockMessage = "This session is held by another local client process; it is released when that process exits."

// localClientLockError preserves ErrLocked without exposing paths or confusing
// the harness PID with the process that owns the OS lock.
type localClientLockError struct{ key string }

func (*localClientLockError) Error() string { return localClientLockMessage }
func (*localClientLockError) Unwrap() error { return clientstate.ErrLocked }

func (o durableOptions) localLockFailure(id broker.AttachRequest, err error) error {
	if !errors.Is(err, clientstate.ErrLocked) {
		return err
	}
	failure := &localClientLockError{}
	if o.state == nil {
		return failure
	}
	key := clientstate.RegistrationKey(o.scope, id.Harness, id.SessionID)
	if o.device != nil {
		clientID, err := o.state.ClientID()
		if err != nil {
			return failure
		}
		key = clientstate.SessionKey(o.scope, id.Harness, id.SessionID, clientID)
	}
	failure.key = key
	// The diagnostics file uses its own lock; never alter the competing
	// client's connection state or release its conn.lock.
	_ = o.state.RecordDiagnostic(key, clientstate.DiagnosticEvent{At: time.Now().UTC(), SessionID: id.SessionID, Event: "reconnect-failed", Reason: "owned-local", ErrorClass: "other"})
	return failure
}

func (o durableOptions) localLockResult(ctx context.Context, upstream Upstream, tool string, failure *localClientLockError) *mcp.CallToolResult {
	if tool != "auth_status" {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: localClientLockMessage}}}
	}
	summary := &clientstate.DiagnosticSummary{Unavailable: true}
	if o.state != nil && failure.key != "" {
		if diagnostics, err := o.state.LoadDiagnostics(failure.key); err == nil {
			diagnostics = clientstate.SafeDiagnostics(diagnostics)
			summary = &diagnostics.Summary
		}
	}
	out := authVersions(ctx, upstream, authStatus{Status: authFailed, Message: localClientLockMessage, ReceiveError: "owned-local", Diagnostics: summary})
	out = o.deviceIdentityStatus(out)
	raw, _ := json.Marshal(out)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}, StructuredContent: out}
}

// Only fixed messages from local registration loading may cross the automatic
// middleware. Arbitrary filesystem/peer errors retain the generic safe wording.
type localStateError struct {
	message string
	cause   error
}

func (e *localStateError) Error() string { return e.message }
func (e *localStateError) Unwrap() error { return e.cause }
func (o durableOptions) localRegistrationFailure(id broker.AttachRequest, err error) error {
	if errors.Is(err, clientstate.ErrLocked) {
		return o.localLockFailure(id, err)
	}
	switch err.Error() {
	case "session state is corrupt; remove it and authorize again",
		"session state is unreadable", "session state could not be written",
		"session state directory could not be created",
		"registration state is corrupt; remove it and register again",
		"registration state is unreadable", "registration state could not be written",
		"registration lock file could not be opened",
		"client id file is invalid; remove it to generate a new identity",
		"client id file is unreadable", "client id file could not be written",
		"client id lock is unavailable":
		return &localStateError{message: err.Error(), cause: err}
	default:
		return err
	}
}
