package broker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func TestSendOnlyPendingSenderPreservesLeaseAndQueueLimits(t *testing.T) {
	for _, kind := range []string{"expired-sender", "expired-recipient", "queue-and-body"} {
		t.Run(kind, func(t *testing.T) {
			s, now := clockedService(t, "")
			s.SetAdmission(gatingAdmission{})
			sender := durableRegister(1, "sender", "client-sender-001", durableSecretA, "lease-sender", "request-sender")
			sender.IngressPending = true
			recipient := durableRegister(2, "recipient", "client-recipient-002", durableSecretB, "lease-recipient", "request-recipient")
			first, err := s.RegisterSession(t.Context(), sender)
			if err != nil || first.Code != "" {
				t.Fatal("sender fixture failed")
			}
			second, err := s.RegisterSession(t.Context(), recipient)
			if err != nil || second.Code != "" {
				t.Fatal("recipient fixture failed")
			}
			ctx := access.WithGrant(context.WithValue(t.Context(), contextKeyOwner{}, "sender"), access.Grant{Scopes: []string{access.MessageSend, access.SessionRegister}})
			message := Message{From: "sender", To: "recipient", Body: "fixture"}
			if kind != "queue-and-body" {
				*now = now.Add(25 * time.Second)
				renew := RenewRequest{RegistrationID: second.Session.RegistrationID, ChannelID: recipient.ChannelID}
				if kind == "expired-recipient" {
					renew = RenewRequest{RegistrationID: first.Session.RegistrationID, ChannelID: sender.ChannelID, IngressPending: true}
				}
				renewed, err := s.RenewLease(t.Context(), renew)
				if err != nil || renewed.Code != "" {
					t.Fatal("live lease fixture failed")
				}
				*now = now.Add(6 * time.Second)
				result, err := s.SendMessage(ctx, message)
				if err == nil || result.Accepted {
					t.Fatal("send-only bypassed expired lease")
				}
				return
			}
			for _, body := range []string{"", strings.Repeat("x", 12001)} {
				invalid := message
				invalid.Body = body
				if sent, err := s.SendMessage(ctx, invalid); err == nil || sent.Accepted {
					t.Fatal("send-only bypassed body limit")
				}
			}
			target, _ := s.GetSession(t.Context(), "recipient")
			if target.PendingMessages != 0 {
				t.Fatal("invalid body changed queue")
			}
			for i := 0; i < 32; i++ {
				sent, err := s.SendMessage(ctx, message)
				if err != nil || !sent.Accepted {
					t.Fatal("valid send-only enqueue failed")
				}
			}
			if sent, err := s.SendMessage(ctx, message); !errors.Is(err, ErrQueueFull) || sent.Accepted {
				t.Fatal("send-only bypassed queue limit")
			}
			target, _ = s.GetSession(t.Context(), "recipient")
			if target.PendingMessages != 32 {
				t.Fatal("queue overflow changed queue")
			}
		})
	}
}
