package serverruntime

import (
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"strings"
	"testing"
)

func TestDirectRuntimeConfirmationFailureReplaysCommittedRegistration(t *testing.T) {
	remote := broker.NewRemoteService()
	defer remote.Close()
	backend := newProtectedBackend("test", remote)
	req := protectedRequest(31, "worker")
	failed := true
	claims, confirms := 0, 0
	grant := access.Grant{Project: "test", TokenID: strings.Repeat("T", 52), ParentID: strings.Repeat("P", 52), AdmissionMode: "auto", OwnerID: "owner", Nickname: "worker", Scopes: access.LegacyV1Scopes(), Claim: func(string) error { return nil }, Check: func(string) error { return nil }, CheckName: func(string, string) error { return nil }}
	grant.Registration = func(identity, id, owner, name string, revision uint64, confirmed bool) error {
		if identity == "" || id == "" || name != "worker" {
			return errors.New("invalid registration evidence")
		}
		if confirmed {
			confirms++
			if failed {
				return errors.New("policy write unavailable")
			}
		} else {
			claims++
		}
		return nil
	}
	ctx := access.WithGrant(t.Context(), grant)
	if _, err := backend.RegisterSession(ctx, req); !errors.Is(err, errProtectedAccess) {
		t.Fatal("failed policy confirmation did not reject response")
	}
	if len(remote.RegistrationOwners()) != 1 {
		t.Fatal("confirmation failure erased committed broker record")
	}
	failed = false
	out, err := backend.RegisterSession(ctx, req)
	if err != nil || out.Code != "" || claims != 1 || confirms != 2 {
		t.Fatal("exact replay did not finish confirmation without another claim")
	}
	originalID := out.Session.RegistrationID
	replayGrant := grant
	replayGrant.TokenID = strings.Repeat("N", 52)
	if _, err = backend.RegisterSession(access.WithGrant(t.Context(), replayGrant), req); !errors.Is(err, errProtectedAccess) {
		t.Fatal("uncommitted grant confirmed another grant's replay")
	}
	resume := broker.ResumeRequest{Holder: g4FixtureHolder(), RegistrationID: originalID, Harness: req.Harness, SessionID: req.SessionID, PID: req.PID, CWD: req.CWD, ClientID: req.ClientID, ChannelID: "direct-resume", Secret: req.Secret, RequestID: "direct-resume", ExpectedRevision: out.Session.Revision}
	again, err := backend.ResumeSession(ctx, resume)
	if err != nil || again.Code != "" || again.Session.RegistrationID != originalID || confirms != 3 {
		t.Fatal("direct resume did not confirm the same registration")
	}
}
