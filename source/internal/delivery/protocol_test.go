package delivery

import (
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"testing"
)

func TestRegistrationProtocolCompatibility(t *testing.T) {
	// Compile-time assignment proves the broker API and client DTO retain exact
	// type identity, including nested sessions/probes and optimistic revisions.
	var request broker.RegisterRequest = protocol.RegisterRequest{ExpectedRevision: 7}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["expectedRevision"] != float64(7) {
		t.Fatalf("stale-registration fence lost on wire: %s", data)
	}
	var result broker.RegisterResult = protocol.RegisterResult{Session: protocol.Session{Name: "assigned"}, PendingProbe: &protocol.PendingProbe{Revision: 9}}
	if result.Session.Name != "assigned" || result.PendingProbe.Revision != 9 {
		t.Fatalf("wire result: %+v", result)
	}
}
