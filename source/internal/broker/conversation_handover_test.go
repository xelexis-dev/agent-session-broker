package broker

import (
	"context"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestConversationHandoverProofAndMutationGuards(t *testing.T) {
	for _, kind := range []string{"secret-mismatch", "client-mismatch", "owner-mismatch", "suspended", "not-found", "still-receiving", "success"} {
		t.Run(kind, func(t *testing.T) {
			s, err := NewPersistentRemoteService(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			s.SetAdmission(&ownerTestAdmission{owner: "owner"})
			ctx := access.WithGrant(context.Background(), access.Grant{OwnerID: "owner", Scopes: []string{access.SessionRegister}})
			old := RegisterRequest{Holder: syntheticReceiverHolder(1, "f"), Harness: "claude", SessionID: "11111111-1111-4111-8111-111111111111", PID: 1001, CWD: "/synthetic", ClientID: "synthetic-client", Name: "worker", ChannelID: "old-lease", Secret: strings.Repeat("s", 32), RequestID: "original"}
			registered, err := s.RegisterSession(ctx, old)
			if err != nil || registered.Code != "" {
				t.Fatal("registration setup")
			}
			id := registered.Session.RegistrationID
			if got := s.byRegistration[id].record.OwnerID; got != "owner" {
				t.Fatalf("original registration owner = %q, want owner", got)
			}
			next := old
			next.SessionID = "22222222-2222-4222-8222-222222222222"
			next.RequestID = "handover"
			next.ExpectedRevision = registered.Session.Revision
			switch kind {
			case "secret-mismatch":
				next.Secret = strings.Repeat("x", 32)
			case "client-mismatch":
				next.ClientID = "another-client"
			case "owner-mismatch":
				ctx = access.WithGrant(context.Background(), access.Grant{OwnerID: "other", Scopes: []string{access.SessionRegister}})
			case "suspended":
				s.byRegistration[id].record.Suspended = true
			case "not-found":
				id = "AAAAAAAAAAAAAAAAAAAAAAAAAA"
			case "still-receiving":
				next.PID = 1002
			}
			_, err = s.RebindConversation(ctx, next, id)
			if kind == "success" {
				if err != nil {
					t.Fatal(err)
				}
				e := s.byRegistration[id]
				if e.record.SessionID != next.SessionID || e.identity.ChannelID != "" || e.record.Revision != 2 || e.record.HandoverFrom == nil {
					t.Fatal("handover did not rebind/fence old lease")
				}
			} else {
				refusal, ok := err.(protocol.HandoverRefusal)
				if !ok || refusal.Reason != kind {
					t.Fatalf("expected refusal %s", kind)
				}
				if s.byRegistration[registered.Session.RegistrationID].record.SessionID != old.SessionID {
					t.Fatal("refusal mutated registration")
				}
			}
		})
	}
}
