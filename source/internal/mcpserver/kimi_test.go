package mcpserver

import (
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

func TestKimiPublicReadinessDoesNotBlockSend(t *testing.T) {
	for _, harness := range []string{"kimi", "claude", "codex"} {
		t.Run(harness, func(t *testing.T) {
			service := broker.NewRemoteService()
			defer service.Close()
			req := broker.RegisterRequest{Holder: g4FixtureHolder(), Harness: harness, SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: "/work", ClientID: "client-kimi-0001", Name: "worker", ChannelID: "lease", Secret: strings.Repeat("s", 40), RequestID: "request"}
			registered, err := service.RegisterSession(t.Context(), req)
			if err != nil || registered.Code != "" || !registered.Session.AutomaticDeliveryReady {
				t.Fatal("internal registration readiness changed")
			}
			client := durableClient(t, service)
			got := callInto[broker.Session](t, client, "get_session", map[string]string{"name": "worker"})
			listed := callInto[[]broker.Session](t, client, "list_sessions", map[string]any{})
			wantReady := harness != "kimi"
			if got.AutomaticDeliveryReady != wantReady || len(listed) != 1 || listed[0].AutomaticDeliveryReady != wantReady {
				t.Fatalf("public readiness must be %v for %s", wantReady, harness)
			}
			internal, err := service.GetSession(t.Context(), "worker")
			if err != nil || !internal.AutomaticDeliveryReady {
				t.Fatal("public read mutated internal readiness")
			}
			sent := callInto[broker.SendResult](t, client, "send_message", map[string]string{"from": "worker", "to": "worker", "body": "still accepted"})
			if !sent.Accepted {
				t.Fatal("public readiness blocked send")
			}
			if harness == "kimi" && (sent.WakeStatus != broker.WakeDegraded || sent.WarningMessage != "queued for the next eligible UserPromptSubmit or Stop hook; idle, failed or continued turns may wait until the next user prompt") {
				t.Fatal("public Kimi delivery warning missing")
			}
			message, err := service.WaitForIngress(t.Context(), "worker", "lease")
			if err != nil || message.Body != "still accepted" {
				t.Fatal("public readiness blocked ingress", err)
			}
		})
	}
}
