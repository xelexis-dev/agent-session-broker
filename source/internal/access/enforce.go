package access

import (
	"context"
	"errors"
)

// ErrInsufficientScope is intentionally fixed: it reveals no token or scope list.
// It is a tool authorization failure, never an invalid-credential signal.
var ErrInsufficientScope = errors.New("insufficient_scope")

// RequireScope narrows authenticated requests only. Local/internal callers
// without a grant keep their existing policy; a grant with missing scopes does
// not acquire legacy defaults. Authentication and object checks remain separate.
func RequireScope(ctx context.Context, scope string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if grant, ok := FromContext(ctx); ok && (scope == "" || !HasScope(grant.Scopes, scope)) {
		return ErrInsufficientScope
	}
	return nil
}
