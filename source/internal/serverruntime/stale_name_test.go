package serverruntime

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"testing"
)

func TestStaleNameOnlySessionBoundGetConfirms(t *testing.T) {
	for _, mode := range []string{"bound", "unbound", "internal", "list"} {
		t.Run(mode, func(t *testing.T) {
			store := protectedStore(t)
			remote := broker.NewRemoteService()
			defer remote.Close()
			b := newProtectedBackend("project", remote)
			sender, ss := manageRegistered(t, store, b, 201, "admin", "sender")
			old, os := manageRegistered(t, store, b, 202, "admin", "recipient")
			sendctx := access.WithGrant(t.Context(), sender)
			if out, err := b.SendMessage(sendctx, broker.Message{From: ss.Name, To: os.Name, Body: "observed"}); err != nil || !out.Accepted {
				t.Fatal(out, err)
			}
			if _, err := b.RenameSession(access.WithGrant(t.Context(), old), broker.RenameRequest{RegistrationID: os.RegistrationID, ChannelID: protectedRequest(202, "recipient").ChannelID, Name: "old-recipient"}); err != nil {
				t.Fatal(err)
			}
			_, replacement := manageRegistered(t, store, b, 203, "admin", "recipient")
			switch mode {
			case "bound":
				if _, err := b.GetSession(sendctx, "recipient"); err != nil {
					t.Fatal(err)
				}
			case "unbound":
				g := manageGrant(t, store, "admin", "observer")
				if _, err := b.GetSession(access.WithGrant(t.Context(), g), "recipient"); err != nil {
					t.Fatal(err)
				}
			case "internal":
				if _, err := b.GetSession(t.Context(), "recipient"); err != nil {
					t.Fatal(err)
				}
			case "list":
				if _, err := b.ListSessions(sendctx); err != nil {
					t.Fatal(err)
				}
			}
			out, err := b.SendMessage(sendctx, broker.Message{From: ss.Name, To: replacement.Name, Body: "reply"})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "bound" {
				if !out.Accepted {
					t.Fatal(out)
				}
			} else if out.Code != "recipient-changed" || out.Accepted {
				t.Fatal("unbound observation unlocked", out)
			}
		})
	}
}
