package channel

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// A Claude session started from inside another Claude session's tool has two
// interactive Claude ancestors. The new session appears in the roster only
// after the MCP client already started, so the client must wait for the
// nearest ancestor's row instead of binding to the outer session.
func TestParentIdentityWaitsForTheNearestClaudeAncestor(t *testing.T) {
	const inner, outer = "11111111-1111-4111-8111-111111111111", "99999999-9999-4999-8999-999999999999"
	var polls atomic.Int32
	roster := func(context.Context) ([]rosterRow, error) {
		n := polls.Add(1)
		rows := []rosterRow{{SessionID: outer, PID: 400, Kind: "interactive"}}
		if n >= 3 {
			rows = append(rows, rosterRow{SessionID: inner, PID: 200, Kind: "interactive"})
		}
		return rows, nil
	}
	inspect := func(_ context.Context, sessionID string) (broker.AttachRequest, error) {
		if sessionID == outer {
			t.Fatal("outer Claude session was inspected as the parent")
		}
		return broker.AttachRequest{Harness: "claude", SessionID: sessionID, PID: 200, CWD: `C:\work`}, nil
	}
	identity, err := nearestClaudeIdentity(context.Background(), 200, roster, inspect, time.Second, 5*time.Millisecond)
	if err != nil || identity.SessionID != inner || identity.PID != 200 {
		t.Fatalf("nearest parent: %+v %v", identity, err)
	}
	if polls.Load() < 3 {
		t.Fatalf("returned before the nearest row appeared after %d polls", polls.Load())
	}
}

func TestParentIdentityFailsWhenTheNearestClaudeNeverAppears(t *testing.T) {
	roster := func(context.Context) ([]rosterRow, error) {
		return []rosterRow{{SessionID: "99999999-9999-4999-8999-999999999999", PID: 400, Kind: "interactive"}, {SessionID: "22222222-2222-4222-8222-222222222222", PID: 201, Kind: "background"}}, nil
	}
	inspect := func(context.Context, string) (broker.AttachRequest, error) {
		t.Fatal("no row of the nearest ancestor should be inspected")
		return broker.AttachRequest{}, nil
	}
	_, err := nearestClaudeIdentity(context.Background(), 200, roster, inspect, 60*time.Millisecond, 5*time.Millisecond)
	if err == nil {
		t.Fatal("bound to a session that is not the nearest Claude ancestor")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := nearestClaudeIdentity(ctx, 200, roster, inspect, time.Second, 5*time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}
