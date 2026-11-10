package serverruntime

import (
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

func TestSendOnlyReadinessPreservesAuthorizationAndRecipientRules(t *testing.T) {
	for _, kind := range []string{"send-only", "legacy", "internal", "foreign-sender", "pending-recipient", "detached-sender", "revoked-sender"} {
		t.Run(kind, func(t *testing.T) {
			store := protectedStore(t)
			grant, _ := protectedGrant(t, store, "sender")
			if kind != "legacy" {
				grant.Scopes = []string{access.MessageSend, access.SessionRegister}
			}
			raw := broker.NewRemoteService()
			defer raw.Close()
			b := newProtectedBackend("project", raw)
			ctx := access.WithGrant(t.Context(), grant)
			req := protectedRequest(201, "ignored")
			req.IngressPending = true
			registered, err := b.RegisterSession(ctx, req)
			if err != nil || registered.Code != "" || registered.Session.AutomaticDeliveryReady {
				t.Fatal("pending sender fixture failed")
			}
			other := protectedRequest(202, "recipient")
			other.IngressPending = kind == "pending-recipient"
			protectedRegistered(t, raw, other)
			from := "sender"
			switch kind {
			case "internal":
				ctx = t.Context()
			case "foreign-sender":
				from = "recipient"
			case "detached-sender":
				if err := raw.StopSession(t.Context(), "sender"); err != nil {
					t.Fatal(err)
				}
			case "revoked-sender":
				if err := store.RevokeToken("project", grant.TokenID); err != nil {
					t.Fatal(err)
				}
			}
			sent, err := b.SendMessage(ctx, broker.Message{From: from, To: "recipient", Body: "fixture"})
			if kind == "send-only" {
				if err != nil || !sent.Accepted {
					t.Fatal("bound send-only pending sender must be allowed")
				}
				message, err := raw.WaitForIngress(t.Context(), "recipient", other.ChannelID)
				if err != nil || message.Body != "fixture" || message.From != "sender" {
					t.Fatal("accepted send-only message was not delivered")
				}
			} else {
				if err == nil || sent.Accepted {
					t.Fatal("send-only exception bypassed existing guard")
				}
				recipient, err := raw.GetSession(t.Context(), "recipient")
				if err != nil || recipient.PendingMessages != 0 {
					t.Fatal("rejected send changed recipient queue")
				}
			}
		})
	}
}
