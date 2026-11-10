package channel

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/claude"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestBackgroundParentIdentityUsesConversationUUID(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	for _, kind := range []string{"background", "interactive"} {
		t.Run(kind, func(t *testing.T) {
			rows := func(context.Context) ([]rosterRow, error) {
				return []rosterRow{{SessionID: id, PID: 200, Kind: kind}}, nil
			}
			got, err := nearestClaudeIdentity(t.Context(), 200, rows, func(_ context.Context, s string) (broker.AttachRequest, error) {
				return broker.AttachRequest{Harness: "claude", SessionID: s, PID: 200, CWD: "/work"}, nil
			}, 20*time.Millisecond, time.Millisecond)
			if err != nil || got.SessionID != id || got.PID != 200 {
				t.Fatalf("parent kind=%s: id=%s error=%v", kind, got.SessionID, err)
			}
		})
	}
}
func TestBackgroundParentIdentityGuards(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	for _, kind := range []string{"other-pid", "duplicate", "invalid-session", "unsupported", "changed-inspection", "changed-session"} {
		t.Run(kind, func(t *testing.T) {
			row := rosterRow{SessionID: id, PID: 200, Kind: "background"}
			rows := []rosterRow{row}
			switch kind {
			case "other-pid":
				rows[0].PID = 201
			case "duplicate":
				rows = append(rows, row)
			case "invalid-session":
				rows[0].SessionID = "1234abcd"
			case "unsupported":
				rows[0].Kind = "worker"
			}
			inspect := func(_ context.Context, s string) (broker.AttachRequest, error) {
				if kind != "changed-inspection" && kind != "changed-session" {
					t.Fatal("unsafe roster inspected")
				}
				if kind == "changed-session" {
					return broker.AttachRequest{Harness: "claude", SessionID: "22222222-2222-4222-8222-222222222222", PID: 200, CWD: "/work"}, nil
				}
				return broker.AttachRequest{Harness: "claude", SessionID: s, PID: 201, CWD: "/work"}, nil
			}
			_, err := nearestClaudeIdentity(t.Context(), 200, func(context.Context) ([]rosterRow, error) { return rows, nil }, inspect, 10*time.Millisecond, time.Millisecond)
			if err == nil {
				t.Fatal("unsafe parent accepted")
			}
		})
	}
}
func TestBackgroundSessionFileFastPath(t *testing.T) {
	for _, kind := range []string{"bg", "interactive", "background", "worker"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", root)
			f, _, id, _ := fastFollowFixture(t)
			path := writeFollowSession(t, root, id)
			data, _ := json.Marshal(map[string]any{"pid": id.PID, "cwd": id.CWD, "kind": kind, "sessionId": id.SessionID})
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			got := f.unchangedSession(id)
			f.mu.Unlock()
			if want := kind == "bg" || kind == "interactive"; got != want {
				t.Fatalf("kind=%s unchanged=%v want=%v", kind, got, want)
			}
		})
	}
}
func TestBackgroundConversationChangeFollowsAndChecksDelivery(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	f, opts, id, _ := fastFollowFixture(t)
	next := claudeHandoverIdentity(711, id.PID)
	path := writeFollowSession(t, root, next)
	data, _ := json.Marshal(map[string]any{"pid": next.PID, "cwd": next.CWD, "kind": "bg", "sessionId": next.SessionID})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	observations, moves := 0, 0
	opts.observe = func(ctx context.Context, _ broker.AttachRequest) (broker.AttachRequest, error) {
		observations++
		return nearestClaudeIdentity(ctx, id.PID, func(context.Context) ([]rosterRow, error) {
			return []rosterRow{{SessionID: next.SessionID, PID: next.PID, Kind: "background"}}, nil
		}, func(context.Context, string) (broker.AttachRequest, error) { return next, nil }, 10*time.Millisecond, time.Millisecond)
	}
	b := f.lookup(id.SessionID)
	sink := f.wrapSink(b, id, func(context.Context, broker.Message) error {
		t.Fatal("old conversation received switched message")
		return nil
	})
	if err := sink(t.Context(), broker.Message{Body: "background"}); err != errFollowDelivery {
		t.Fatalf("delivery switch not detected: %v", err)
	}
	// The ordinary follow transition, with the same stability/process-key gate.
	b.followMessage = nil
	f.move = func(_ context.Context, _, to broker.AttachRequest) (broker.AttachRequest, error) {
		moves++
		return to, nil
	}
	got, err := f.check(t.Context(), id)
	if err != nil || got.SessionID != next.SessionID || moves != 1 || observations < 8 {
		t.Fatalf("background follow failed: moved=%d observations=%d err=%v", moves, observations, err)
	}
}

type backgroundRosterExecutor struct{ raw string }

func (e backgroundRosterExecutor) Run(context.Context, claude.Call) (string, error) {
	return e.raw, nil
}
func TestBackgroundClientInspectorMapsFullSessionID(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	raw, _ := json.Marshal([]map[string]any{{"id": "1234abcd", "sessionId": id, "pid": os.Getpid(), "kind": "background", "cwd": t.TempDir(), "status": "idle"}})
	got, err := inspectClaudeParent(t.Context(), backgroundRosterExecutor{string(raw)}, os.Getpid())
	if err != nil || got.SessionID != id || got.PID != os.Getpid() {
		t.Fatalf("client parent lookup failed: %+v %v", got, err)
	}
}
func TestBackgroundSpareWaitsForExplicitAttach(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	r := newDetachedRuntime(t)
	id := claudeHandoverIdentity(720, 9301)
	path := writeFollowSession(t, root, id)
	data, _ := json.Marshal(map[string]any{"pid": id.PID, "cwd": id.CWD, "kind": "bg", "sessionId": id.SessionID})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	h := r.identityHarness(t, t.TempDir(), id, func(o *durableOptions) {
		handoverOptions(o)
		o.claudeStartupClock = instantClaudeClock()
		o.observe = func(ctx context.Context, _ broker.AttachRequest) (broker.AttachRequest, error) {
			return nearestClaudeIdentity(ctx, id.PID, func(context.Context) ([]rosterRow, error) {
				return []rosterRow{{SessionID: id.SessionID, PID: id.PID, Kind: "background"}}, nil
			}, func(context.Context, string) (broker.AttachRequest, error) { return id, nil }, time.Second, time.Millisecond)
		}
	})
	for i := 0; i < 3; i++ {
		if h.status(t).Registered {
			t.Fatal("spare background process registered automatically")
		}
	}
	if r.direct.Load() != 0 {
		t.Fatal("spare background process started admission")
	}
	h.attach(t)
	if !h.status(t).Registered {
		t.Fatal("explicit background attach failed")
	}
}

func TestBackgroundDoneParentFollowAndDeliveryStayBound(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // Force the full observer path.
	f, opts, id, _ := fastFollowFixture(t)
	id.PID = os.Getpid()
	f.initial = id
	b := f.lookup(id.SessionID)
	b.identity = id
	raw, _ := json.Marshal([]map[string]any{{"id": "1234abcd", "sessionId": id.SessionID, "pid": id.PID, "kind": "background", "cwd": id.CWD, "status": "idle", "state": "done"}})
	observations, failed := 0, 0
	opts.observe = func(ctx context.Context, _ broker.AttachRequest) (broker.AttachRequest, error) {
		observations++
		got, err := inspectClaudeParent(ctx, backgroundRosterExecutor{string(raw)}, id.PID)
		if err != nil {
			failed++
		}
		return got, err
	}
	f.move = func(context.Context, broker.AttachRequest, broker.AttachRequest) (broker.AttachRequest, error) {
		t.Fatal("unchanged background parent moved")
		return id, nil
	}
	got, err := f.check(t.Context(), id)
	if err != nil || got.SessionID != id.SessionID {
		t.Fatal("follow lost idle background parent")
	}
	delivered := 0
	sink := f.wrapSink(b, id, func(context.Context, broker.Message) error { delivered++; return nil })
	if err := sink(t.Context(), broker.Message{}); err != nil {
		t.Fatal(err)
	}
	if observations != 2 || failed != 0 || delivered != 1 || !b.started || b.receiveReason != "" {
		t.Fatalf("idle background recheck failed: observations=%d failed=%d delivered=%d", observations, failed, delivered)
	}
}
