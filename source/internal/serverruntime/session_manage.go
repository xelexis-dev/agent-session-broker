package serverruntime

import (
	"context"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func (b *protectedBackend) managementCheck(ctx context.Context, kind string) broker.ManagementCheck {
	return func(target broker.Session, owner string, callers []broker.Session) (protocol.ManagementEvent, error) {
		grant, granted := access.FromContext(ctx)
		if !granted {
			return protocol.ManagementEvent{ActorName: "panel"}, nil
		}
		if grant.Project != b.project || grant.TokenID == "" {
			return protocol.ManagementEvent{}, errProtectedAccess
		}
		var caller broker.Session
		for _, candidate := range callers {
			if b.CheckSession(ctx, candidate) == nil {
				caller = candidate
				break
			}
		}
		if caller.RegistrationID == "" || caller.Suspended || caller.ErrorCode == broker.CodeDetached {
			return protocol.ManagementEvent{}, errProtectedAccess
		}
		event := protocol.ManagementEvent{ActorName: caller.Name, ActorRegistrationID: caller.RegistrationID}
		if caller.RegistrationID == target.RegistrationID {
			if kind != "detached" {
				return event, errProtectedAccess
			}
			return event, nil
		}
		if grant.OwnerID == "" || grant.Manage == nil || grant.Manage(bindingIdentity(target.Harness, target.ID, target.ClientID), owner) != nil {
			return event, errProtectedAccess
		}
		return event, nil
	}
}
func (b *protectedBackend) manageSession(ctx context.Context, selector, kind string) error {
	if err := access.RequireScope(ctx, access.SessionManage); err != nil {
		return err
	}
	check := b.managementCheck(ctx, kind)
	if err := b.RemoteService.CheckManagementAccess(ctx, selector, check); err != nil {
		if _, granted := access.FromContext(ctx); granted && errors.Is(err, broker.ErrSessionNotFound) {
			return errProtectedAccess
		}
		return err
	}
	grant, _ := access.FromContext(ctx)
	audit := func(event protocol.ManagementEvent) error {
		if kind == "suspended" && grant.CancelSuspendedRequests != nil {
			grant.CancelSuspendedRequests(event.TargetName)
		}
		if grant.ManagementAudit != nil {
			return grant.ManagementAudit(event)
		}
		return nil
	}
	return b.RemoteService.ManageSession(ctx, selector, kind, check, audit)
}
func (b *protectedBackend) SuspendSession(ctx context.Context, selector string) error {
	return b.manageSession(ctx, selector, "suspended")
}
func (b *protectedBackend) UnsuspendSession(ctx context.Context, selector string) error {
	return b.manageSession(ctx, selector, "unsuspended")
}
