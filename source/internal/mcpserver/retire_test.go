package mcpserver

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"testing"
)

func TestRetireScope(t *testing.T) {
	if toolScope("retire_session") != access.SessionManage {
		t.Fatal("retire not protected by session:manage")
	}
}

func TestRetireAdvertisedOnlyForRemoteBackend(t *testing.T) {
	remote := broker.NewRemoteService()
	defer remote.Close()
	for _, tc := range []struct {
		backend Backend
		want    bool
	}{{remote, true}, {stubBackend{}, false}} {
		listed, err := catalogSession(t, New(tc.backend, &stubMemory{})).ListTools(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, tool := range listed.Tools {
			found = found || tool.Name == "retire_session"
		}
		if found != tc.want {
			t.Fatal("wrong retire catalog visibility", found, tc.want)
		}
	}
}
