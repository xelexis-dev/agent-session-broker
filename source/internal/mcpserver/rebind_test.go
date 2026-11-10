package mcpserver

import (
	"context"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type rebindTestBackend struct{ *broker.RemoteService }

func (b rebindTestBackend) RebindSession(context.Context, protocol.RebindRequest) (protocol.RebindResult, error) {
	return protocol.RebindResult{Reason: "not-found"}, nil
}
func TestRebindSessionIsCallableInternalAndScoped(t *testing.T) {
	remote := broker.NewRemoteService()
	defer remote.Close()
	client := durableClient(t, rebindTestBackend{remote})
	listed, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Name == "rebind_session" {
			t.Fatal("internal rebind tool advertised")
		}
	}
	result := callInto[protocol.RebindResult](t, client, "rebind_session", protocol.RebindRequest{})
	if result.Reason != "not-found" {
		t.Fatal("hidden handler could not be called")
	}
	if toolScope("rebind_session") != access.SessionRegister {
		t.Fatal("rebind scope classification changed")
	}
}
