package serverruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func (b *protectedBackend) RebindSession(ctx context.Context, r protocol.RebindRequest) (protocol.RebindResult, error) {
	if err := access.RequireScope(ctx, access.SessionRegister); err != nil {
		return protocol.RebindResult{}, err
	}
	g, ok := access.FromContext(ctx)
	identity := bindingIdentity(r.Harness, r.SessionID, r.ClientID)
	if !ok || g.Project != b.project || g.ParentID == "" || g.AdmissionMode == "auto" || g.Check == nil || g.Rebind == nil || g.OwnerID == "" || g.Check(identity) != nil {
		return protocol.RebindResult{}, errProtectedAccess
	}
	j := protocol.RebindJournal{RequestID: r.RequestID, TokenID: g.TokenID, RegistrationID: r.RegistrationID, ExpectedRevision: r.ExpectedRevision, TemporaryNickname: g.Nickname}
	if result, applied := b.RemoteService.InspectRebindResult(r.RegistrationID, r.RequestID, g.TokenID, r.ExpectedRevision); applied {
		return result, g.Rebind("confirm", j, result)
	}
	proof, err := b.RemoteService.InspectConversationHandover(ctx, r.RegistrationRequest(), r.RegistrationID)
	if err != nil {
		var refusal protocol.HandoverRefusal
		if errors.As(err, &refusal) {
			if abort := g.Rebind("abort", j, protocol.RebindResult{}); abort != nil {
				return protocol.RebindResult{}, abort
			}
		}
		return protocol.RebindResult{}, err
	}
	reason := ""
	if proof.PreviousIdentity == identity {
		reason = "identity-conflict"
	} else if proof.OwnerID != g.OwnerID {
		reason = "owner-mismatch"
	} else if proof.Revision != r.ExpectedRevision {
		reason = "revision-mismatch"
	}
	if reason != "" {
		if abort := g.Rebind("abort", j, protocol.RebindResult{}); abort != nil {
			return protocol.RebindResult{}, abort
		}
		return protocol.RebindResult{}, protocol.HandoverRefusal{Reason: reason}
	}
	source := sha256.Sum256([]byte(proof.PreviousIdentity))
	j.SourceDigest = hex.EncodeToString(source[:])
	j.Name = proof.Name
	if err = g.Rebind("pending", j, protocol.RebindResult{}); err != nil {
		return protocol.RebindResult{}, err
	}
	// The approved credential owns C1 but still has its temporary nickname.
	// Require the exact new identity and broker-observed old name until confirm.
	clone := g
	clone.CheckName = func(bound, name string) error {
		if bound != identity || name != proof.Name {
			return errProtectedAccess
		}
		return g.Check(bound)
	}
	applied, err := b.RemoteService.RebindConversation(access.WithGrant(ctx, clone), r.RegistrationRequest(), r.RegistrationID)
	if err != nil {
		var refusal protocol.HandoverRefusal
		if errors.As(err, &refusal) {
			if abort := g.Rebind("abort", j, protocol.RebindResult{}); abort != nil {
				return protocol.RebindResult{}, abort
			}
		}
		return protocol.RebindResult{}, err
	}
	result := protocol.RebindResult{RegistrationID: applied.RegistrationID, Name: applied.Name, Revision: applied.Revision, HandoverFrom: applied.HandoverFrom}
	if err = g.Rebind("confirm", j, result); err != nil {
		return protocol.RebindResult{}, err
	}
	return result, nil
}
func recoverManualRebinds(store *controlpanel.Store, factory interface {
	RemoteBackend(context.Context, string) (mcpserver.Backend, error)
}) error {
	for _, pending := range store.PendingRebinds() {
		service, err := factory.RemoteBackend(context.Background(), pending.Project)
		if err != nil {
			return err
		}
		inspector, ok := service.(interface {
			InspectRebindResult(string, string, string, uint64) (protocol.RebindResult, bool)
		})
		if !ok {
			return broker.ErrSessionNotFound
		}
		j := pending.Journal
		result, applied := inspector.InspectRebindResult(j.RegistrationID, j.RequestID, j.TokenID, j.ExpectedRevision)
		if err = store.RecoverRebind(pending.Project, j, result, applied); err != nil {
			return err
		}
	}
	return nil
}
