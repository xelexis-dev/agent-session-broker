package mcpserver

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpconfig"
	"testing"
)

func TestInitializeRecommendsPublishedClientVersion(t *testing.T) {
	remote := broker.NewRemoteService()
	defer remote.Close()
	client := catalogSession(t, New(remote, &stubMemory{}, "0.19.9"))
	if got := client.InitializeResult().Capabilities.Experimental["recommendedClientVersion"]; got != mcpconfig.RecommendedClientVersion {
		t.Fatal("server recommendation missing or tied to server version", got)
	}
}
