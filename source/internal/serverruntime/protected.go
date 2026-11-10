package serverruntime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
)

var errProtectedAccess = broker.ErrSessionIdentityUnauthorized

// protectedBackend keeps token authorization at the runtime boundary. The
// admission callbacks also run inside the broker mutation boundary, so a name
// lookup performed here is not the final authorization of a mutable record.
type protectedBackend struct {
	*broker.RemoteService
	project string
}

var _ broker.DurableRegistrar = (*protectedBackend)(nil)
var _ mcpserver.Backend = (*protectedBackend)(nil)

func newProtectedBackend(project string, remote *broker.RemoteService) *protectedBackend {
	b := &protectedBackend{RemoteService: remote, project: project}
	remote.SetAdmission(b)
	return b
}

// JSON array encoding prevents delimiter collisions between identity fields.
func bindingIdentity(harness, sessionID, clientID string) string {
	raw, _ := json.Marshal([3]string{harness, sessionID, clientID})
	return string(raw)
}
func (b *protectedBackend) Reserved(ctx context.Context) bool {
	_, ok := access.FromContext(ctx)
	return ok
}
func (b *protectedBackend) Admit(ctx context.Context, r broker.RegisterRequest) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	grant, ok := access.FromContext(ctx)
	if !ok {
		return r.Name, "", nil
	}
	if grant.Project != b.project || grant.TokenID == "" || grant.Nickname == "" || grant.Claim == nil || grant.Check == nil {
		return "", broker.CodeInvalidToken, nil
	}
	// Name selection must not bind. Core validates an existing record's resume
	// secret and resolves slot conflicts before calling Claim.
	return grant.Nickname, "", nil
}
func (b *protectedBackend) Claim(ctx context.Context, r broker.RegisterRequest) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	grant, ok := access.FromContext(ctx)
	if !ok {
		return "", nil
	}
	if grant.Project != b.project || grant.TokenID == "" || grant.Claim == nil {
		return broker.CodeInvalidToken, nil
	}
	if err := grant.Claim(bindingIdentity(r.Harness, r.SessionID, r.ClientID)); err != nil {
		return broker.CodeInvalidToken, nil
	}
	return "", nil
}

// CheckSession is the broker's optional callback immediately before operating
// on a stored session. It never claims an unbound token.
func (b *protectedBackend) CheckSession(ctx context.Context, s broker.Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	grant, ok := access.FromContext(ctx)
	if !ok {
		return nil
	}
	if grant.Project != b.project || grant.TokenID == "" || s.RegistrationID == "" || s.ClientID == "" || grant.Check == nil {
		return errProtectedAccess
	}
	if grant.CheckName != nil {
		if err := grant.CheckName(bindingIdentity(s.Harness, s.ID, s.ClientID), s.Name); err != nil {
			return errProtectedAccess
		}
	} else if grant.Nickname != s.Name {
		return errProtectedAccess
	}
	if err := grant.Check(bindingIdentity(s.Harness, s.ID, s.ClientID)); err != nil {
		return errProtectedAccess
	}
	return nil
}
func (b *protectedBackend) check(ctx context.Context, selector string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := access.FromContext(ctx); !ok {
		return nil
	}
	err := b.RemoteService.CheckSessionAccess(ctx, selector)
	if errors.Is(err, broker.ErrSessionNotFound) {
		// A removed registration is a terminal recovery result, not a
		// transient tool failure that the client should retry indefinitely.
		return errProtectedAccess
	}
	return err
}
func (b *protectedBackend) AttachSession(ctx context.Context, r broker.AttachRequest) (broker.Session, error) {
	if err := access.RequireScope(ctx, access.SessionRegister); err != nil {
		return broker.Session{}, err
	}
	if err := ctx.Err(); err != nil {
		return broker.Session{}, err
	}
	if b.Reserved(ctx) {
		return broker.Session{}, errProtectedAccess
	}
	return b.RemoteService.AttachSession(ctx, r)
}

func (b *protectedBackend) RegisterSession(ctx context.Context, r broker.RegisterRequest) (broker.RegisterResult, error) {
	if err := access.RequireScope(ctx, access.SessionRegister); err != nil {
		return broker.RegisterResult{}, err
	}
	result, err := b.RemoteService.RegisterSession(ctx, r)
	if err != nil || result.Code != "" {
		return result, err
	}
	if grant, ok := access.FromContext(ctx); ok && grant.Registration != nil {
		if !b.RemoteService.RegistrationAdmissionMatches(result.Session.RegistrationID, grant.TokenID) {
			return broker.RegisterResult{}, errProtectedAccess
		}
		if err := grant.Registration(bindingIdentity(result.Session.Harness, result.Session.ID, result.Session.ClientID), result.Session.RegistrationID, grant.OwnerID, result.Session.Name, result.Session.Revision, true); err != nil {
			return broker.RegisterResult{}, errProtectedAccess
		}
	}
	if grant, ok := access.FromContext(ctx); ok && grant.Connected != nil {
		if err := grant.Connected(bindingIdentity(result.Session.Harness, result.Session.ID, result.Session.ClientID)); err != nil {
			// The pending request can replay the committed registration if the
			// separate policy store could not persist its first connection yet.
			return broker.RegisterResult{}, errProtectedAccess
		}
	}
	return result, nil
}
func (b *protectedBackend) SendMessage(ctx context.Context, m broker.Message) (broker.SendResult, error) {
	if err := access.RequireScope(ctx, access.MessageSend); err != nil {
		return broker.SendResult{}, err
	}
	if err := b.check(ctx, m.From); err != nil {
		return broker.SendResult{}, err
	}
	return b.RemoteService.SendMessage(ctx, m)
}
func (b *protectedBackend) WaitForMessage(ctx context.Context, recipient string) (broker.Message, error) {
	if err := access.RequireScope(ctx, access.MessageReceive); err != nil {
		return broker.Message{}, err
	}
	if err := b.check(ctx, recipient); err != nil {
		return broker.Message{}, err
	}
	return b.RemoteService.WaitForMessage(ctx, recipient)
}
func (b *protectedBackend) WaitForIngressAck(ctx context.Context, r protocol.IngressRequest) (broker.Message, error) {
	if err := access.RequireScope(ctx, access.MessageReceive); err != nil {
		return broker.Message{}, err
	}
	if err := b.check(ctx, r.Recipient); err != nil {
		return broker.Message{}, err
	}
	return b.RemoteService.WaitForIngressAck(ctx, r)
}
func (b *protectedBackend) WaitForIngress(ctx context.Context, recipient, lease string) (broker.Message, error) {
	if err := access.RequireScope(ctx, access.MessageReceive); err != nil {
		return broker.Message{}, err
	}
	if err := b.check(ctx, recipient); err != nil {
		return broker.Message{}, err
	}
	return b.RemoteService.WaitForIngress(ctx, recipient, lease)
}
func (b *protectedBackend) ReportIngressFailure(ctx context.Context, selector, lease string) error {
	if err := access.RequireScope(ctx, access.SessionRegister); err != nil {
		return err
	}
	if err := b.check(ctx, selector); err != nil {
		return err
	}
	return b.RemoteService.ReportIngressFailure(ctx, selector, lease)
}
func (b *protectedBackend) StopSession(ctx context.Context, selector string) error {
	if _, granted := access.FromContext(ctx); !granted {
		return b.RemoteService.StopSession(ctx, selector)
	}
	return b.manageSession(ctx, selector, "detached")
}

func (b *protectedBackend) ResumeSession(ctx context.Context, r broker.ResumeRequest) (broker.RegisterResult, error) {
	if err := access.RequireScope(ctx, access.SessionRegister); err != nil {
		return broker.RegisterResult{}, err
	}
	if err := b.check(ctx, r.RegistrationID); err != nil {
		if errors.Is(err, errProtectedAccess) {
			return broker.RegisterResult{Code: broker.CodeInvalidToken}, nil
		}
		return broker.RegisterResult{}, err
	}
	result, err := b.RemoteService.ResumeSession(ctx, r)
	if err == nil && result.Code == "" {
		if grant, ok := access.FromContext(ctx); ok && grant.Registration != nil {
			if !b.RemoteService.RegistrationAdmissionMatches(result.Session.RegistrationID, grant.TokenID) {
				return broker.RegisterResult{}, errProtectedAccess
			}
			if e := grant.Registration(bindingIdentity(result.Session.Harness, result.Session.ID, result.Session.ClientID), result.Session.RegistrationID, grant.OwnerID, result.Session.Name, result.Session.Revision, true); e != nil {
				return broker.RegisterResult{}, errProtectedAccess
			}
		}
	}
	return result, err
}
func (b *protectedBackend) RenewLease(ctx context.Context, r broker.RenewRequest) (broker.RegisterResult, error) {
	if err := access.RequireScope(ctx, access.SessionRegister); err != nil {
		return broker.RegisterResult{}, err
	}
	if err := b.check(ctx, r.RegistrationID); err != nil {
		if errors.Is(err, errProtectedAccess) {
			return broker.RegisterResult{Code: broker.CodeInvalidToken}, nil
		}
		return broker.RegisterResult{}, err
	}
	return b.RemoteService.RenewLease(ctx, r)
}
func (b *protectedBackend) AnswerProbe(ctx context.Context, r broker.ProbeAnswer) (broker.ProbeResult, error) {
	if err := access.RequireScope(ctx, access.SessionRegister); err != nil {
		return broker.ProbeResult{}, err
	}
	if err := b.check(ctx, r.RegistrationID); err != nil {
		if errors.Is(err, errProtectedAccess) {
			return broker.ProbeResult{Code: broker.CodeInvalidToken}, nil
		}
		return broker.ProbeResult{}, err
	}
	return b.RemoteService.AnswerProbe(ctx, r)
}

// RegistrationOwner exposes only the owner of an authenticated server grant.
// The broker calls this after its final admission/identity checks.
func (b *protectedBackend) RegistrationOwner(ctx context.Context) string {
	grant, ok := access.FromContext(ctx)
	if !ok || grant.Project != b.project || grant.TokenID == "" {
		return ""
	}
	return grant.OwnerID
}

// Roster reads are distinct from the bound sender lookup needed by send_message.
func (b *protectedBackend) ListSessions(ctx context.Context) ([]broker.Session, error) {
	if err := access.RequireScope(ctx, access.SessionRead); err != nil {
		return nil, err
	}
	return b.RemoteService.ListSessions(ctx)
}
func (b *protectedBackend) GetSession(ctx context.Context, selector string) (broker.Session, error) {
	if err := access.RequireScope(ctx, access.SessionRead); err != nil {
		return broker.Session{}, err
	}
	return b.RemoteService.GetSessionObserved(ctx, selector)
}
func (b *protectedBackend) MessageSender(ctx context.Context, selector string) (broker.Session, error) {
	if err := access.RequireScope(ctx, access.MessageSend); err != nil {
		return broker.Session{}, err
	}
	if err := b.check(ctx, selector); err != nil {
		return broker.Session{}, err
	}
	return b.RemoteService.GetSession(ctx, selector)
}
func (b *protectedBackend) RenameSession(ctx context.Context, r broker.RenameRequest) (broker.Session, error) {
	if err := access.RequireScope(ctx, access.SessionRename); err != nil {
		return broker.Session{}, err
	}
	v, err := b.RemoteService.RenameSession(ctx, r)
	if errors.Is(err, controlpanel.ErrNicknameReservedBound) {
		return v, protocol.RenameRejection{Code: "nickname-in-use", Holder: "orphaned-credential"}
	}
	if errors.Is(err, controlpanel.ErrNicknameReservedUnbound) {
		return v, protocol.RenameRejection{Code: "nickname-in-use", Holder: "unused-credential"}
	}
	return v, err
}
