// Package access carries admission policy without depending on a broker or UI.
package access

import (
	"context"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"time"
)

// RetirementTarget is server-owned and used only under the broker mutation lock.
type RetirementTarget struct {
	Identity, Name, RecordedOwner, RegistrationID string
	Event                                         protocol.ManagementEvent
}

type Grant struct {
	Retire func(RetirementTarget, func() error) ([]protocol.RevokedCredential, error)

	ProjectGenerationID string // Authenticated server-owned project lifetime.
	// Rebind coordinates a durable manual-handover journal with policy. Server only.
	Rebind func(phase string, journal protocol.RebindJournal, result protocol.RebindResult) error

	// Manage rechecks live token and web management policy for another registration.
	// Called under the broker mutation lock; arguments are server-owned.
	Manage                  func(identity, recordedOwner string) error
	ManagementAudit         func(protocol.ManagementEvent) error
	CancelSuspendedRequests func(nickname string)

	// Registration is server-only admission/confirmation for direct grants.
	// The false phase runs under the broker mutation lock before its commit.
	Registration            func(identity, id, owner, name string, revision uint64, confirmed bool) error
	ParentID, AdmissionMode string
	// Scopes is a server-owned copy of the persisted child permission snapshot.
	Scopes                     []string
	ExpiresAt                  time.Time
	Revoked                    <-chan struct{}
	Project, TokenID, Nickname string
	// OwnerID is assigned by the server policy store, never decoded from a client.
	OwnerID string
	// Claim binds this token to the canonical client+harness+session identity.
	// Use only for admission after validating ownership of any existing record.
	// It rechecks live policy each time.
	Claim func(identity string) error
	// Connected confirms the first successful durable registration. Admission
	// alone must not prevent an unused token from expiring.
	Connected func(identity string) error
	// Check requires an existing matching binding and never binds an identity.
	// Use before operations on an already registered session.
	Check func(identity string) error
	// CheckName revalidates the current durable name, including recovery of an
	// interrupted rename. It is called under the broker's registration lock.
	CheckName func(identity, name string) error
	// Rename reserves the new name durably, applies the registration write,
	// then finalizes policy. apply must not call back into this policy store.
	Rename func(identity, oldName, newName string, apply func() error) error
}
type grantKey struct{}

func WithGrant(ctx context.Context, grant Grant) context.Context {
	return context.WithValue(ctx, grantKey{}, grant)
}
func FromContext(ctx context.Context) (Grant, bool) {
	g, ok := ctx.Value(grantKey{}).(Grant)
	return g, ok
}
