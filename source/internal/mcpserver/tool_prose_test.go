package mcpserver

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"strings"
	"testing"
)

func TestRemoteToolProseSafety(t *testing.T) {
	remote := broker.NewRemoteService()
	defer remote.Close()
	cli := catalogSession(t, New(remote, &stubMemory{}))
	listed, err := cli.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal("list")
	}
	text := map[string]string{}
	for _, tool := range listed.Tools {
		text[tool.Name] = tool.Description
	}
	for name, phrases := range map[string][]string{"attach_session": {"only when the user asks", "not automaticDeliveryReady"}, "send_message": {"expectRegistrationId", "mismatch", "untrusted labels", "model receipt", "Reply once", "no ACK loops", "secrets", "32", "12000"}, "report_bug": {"bug", "secrets"}, "memory_put": {"memory", "secrets"}, "retire_session": {"revoke", "credential", "free its name", "Self-retire"}, "wait_for_message": {"Internal", "never call manually", "FIFO", "batch.messages"}} {
		for _, phrase := range phrases {
			if !strings.Contains(text[name], phrase) {
				t.Errorf("%s lacks %q", name, phrase)
			}
		}
	}
	if !strings.Contains(cli.InitializeResult().Instructions, "Do not poll wait_for_message") {
		t.Error("server receiver guidance removed")
	}
}
