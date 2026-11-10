package mcpserver

import (
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestMessageAckWireNegotiationAndLegacyProjection(t *testing.T) {
	for _, ack := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "ack"}[ack], func(t *testing.T) {
			service := broker.NewRemoteService()
			defer service.Close()
			client := durableClient(t, service)
			if client.InitializeResult().Capabilities.Experimental[protocol.MessageAckCapability] != true {
				t.Fatal("missing ack capability")
			}
			request := broker.RegisterRequest{Harness: "claude", SessionID: "11111111-1111-4111-8111-111111111111", ClientID: "ack-wire-client", PID: 123, CWD: "/fixture", Name: "receiver", ChannelID: "wire-lease", Secret: "synthetic-wire-secret-01234567890123456789", RequestID: "wire-register", Capabilities: []string{protocol.MessageSentAtCapability}}
			if ack {
				request.Capabilities = append(request.Capabilities, protocol.MessageAckCapability)
			}
			registered := callInto[broker.RegisterResult](t, client, "register_session", request)
			if registered.Code != "" || registered.MessageAckVersion != map[bool]int{false: 0, true: 1}[ack] {
				t.Fatal("wire negotiation")
			}
			for _, body := range []string{"m1", "m2"} {
				if _, err := service.SendMessage(t.Context(), broker.Message{From: request.Name, To: request.Name, Body: body}); err != nil {
					t.Fatal(err)
				}
			}
			wait := protocol.IngressRequest{Recipient: request.Name, ChannelID: request.ChannelID}
			if ack {
				wait.RegistrationID = registered.Session.RegistrationID
				wait.Epoch = registered.Session.Epoch
			}
			first := callInto[broker.Message](t, client, "wait_for_message", wait)
			if first.Body != "m1" || first.DeliverySequence != map[bool]uint64{false: 0, true: 1}[ack] {
				t.Fatal("wire delivery")
			}
			if ack {
				bad := wait
				bad.AckThrough = 1
				bad.Cursor = 2
				result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "wait_for_message", Arguments: bad})
				if err == nil && !result.IsError {
					t.Fatal("future cursor accepted")
				}
				replay := callInto[broker.Message](t, client, "wait_for_message", wait)
				if replay.DeliverySequence != 1 || replay.Body != "m1" {
					t.Fatal("invalid cursor committed ack")
				}
				renewed := callInto[broker.RegisterResult](t, client, "renew_lease", broker.RenewRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: request.ChannelID, Epoch: registered.Session.Epoch, AckThrough: 1})
				if renewed.Code != "" || renewed.MessageAckVersion != 1 {
					t.Fatal("wire renew ack")
				}
				wait.Cursor, wait.AckThrough = 1, 1
			} else {
				expected := first
				expected.DeliverySequence = 0
				a, _ := json.Marshal(first)
				b, _ := json.Marshal(expected)
				if string(a) != string(b) {
					t.Fatal("legacy envelope bytes changed")
				}
			}
			second := callInto[broker.Message](t, client, "wait_for_message", wait)
			if second.Body != "m2" {
				t.Fatal("wire prefix delivery")
			}
		})
	}
}
