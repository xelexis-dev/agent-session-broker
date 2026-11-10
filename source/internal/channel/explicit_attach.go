package channel

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

var errExplicitBindingStale = errors.New("ingress binding changed; retry attach_session")

// Only definitive remote absence retires a live binding; transport failures do not.
func explicitRegistrationProof(ctx context.Context, u Upstream, registration string) error {
	request := &mcp.CallToolParams{Name: "get_session", Arguments: map[string]string{"name": broker.SelectorRegistration + registration}}
	result, err := u.CallTool(ctx, request)
	if errors.Is(err, mcp.ErrConnectionClosed) {
		// Cancellation can leave a closed cached session; the first read discards it.
		result, err = u.CallTool(ctx, request)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("broker get_session transport failed")
	}
	if result.IsError {
		for _, item := range result.Content {
			if text, ok := item.(*mcp.TextContent); ok && text.Text == "session not found" {
				return errExplicitBindingStale
			}
		}
		return errors.New("broker get_session rejected the request")
	}
	raw, err := json.Marshal(result.StructuredContent)
	if result.StructuredContent == nil && len(result.Content) == 1 {
		if text, ok := result.Content[0].(*mcp.TextContent); ok {
			raw = []byte(text.Text)
		}
	}
	var current broker.Session
	if err != nil || json.Unmarshal(raw, &current) != nil || current.RegistrationID != registration {
		return errors.New("invalid registration proof")
	}
	if current.ErrorCode == broker.CodeDetached {
		return errExplicitBindingStale
	}
	return nil
}

func (o durableOptions) explicitCredentialRejection(a *sessionAuth) error {
	o.rejectCredential(a)
	a.mu.Lock()
	automatic := a.autoEligibleLocked()
	a.mu.Unlock()
	if _, parent := o.device.authorizer.(directAuthorizer); parent && automatic {
		return errExplicitBindingStale
	}
	return errors.New(a.pendingText())
}
