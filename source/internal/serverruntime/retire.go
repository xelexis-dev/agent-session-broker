package serverruntime

import (
	"context"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func (b *protectedBackend) RetireSession(ctx context.Context, r protocol.RetireRequest) (protocol.RetireResult, error) {
	if err := access.RequireScope(ctx, access.SessionManage); err != nil {
		return protocol.RetireResult{}, err
	}
	g, ok := access.FromContext(ctx)
	if !ok || g.Project != b.project || g.Retire == nil {
		return protocol.RetireResult{}, protocol.RejectRetire("forbidden")
	}
	check := func(target broker.Session, owner string, callers []broker.Session) (protocol.ManagementEvent, error) {
		var caller broker.Session
		for _, c := range callers {
			if b.CheckSession(ctx, c) == nil {
				caller = c
				break
			}
		}
		if caller.RegistrationID == "" || caller.Suspended || caller.ErrorCode == broker.CodeDetached {
			return protocol.ManagementEvent{}, protocol.RejectRetire("forbidden")
		}
		if target.RegistrationID == caller.RegistrationID {
			return protocol.ManagementEvent{}, protocol.RejectRetire("self-retire-denied")
		}
		if target.RegistrationID == "" {
			return protocol.ManagementEvent{}, nil
		}
		event, err := b.managementCheck(ctx, "retired")(target, owner, callers)
		if err != nil {
			return event, protocol.RejectRetire("forbidden")
		}
		return event, nil
	}
	return b.RemoteService.RetireManaged(ctx, r, check, g.Retire)
}
