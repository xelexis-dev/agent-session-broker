package localactivity

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const sessionA = "11111111-1111-4111-8111-111111111111"
const sessionB = "22222222-2222-4222-8222-222222222222"

type fixture struct {
	t        testing.TB
	config   Config
	now      time.Time
	path     string
	observer *Observer
	process  Process
}

func newFixture(t testing.TB, harness string) *fixture {
	t.Helper()
	f := &fixture{t: t, now: time.Date(2026, 10, 10, 6, 0, 0, 0, time.UTC), process: Process{Key: "creation-a", RecordStart: "123456", Host: "host", Platform: "test"}}
	f.config = Config{Binding: Binding{Harness: harness, SessionID: sessionA, ClientID: "pc", RegistrationID: "registration", LeaseID: "lease", Generation: 1}, Home: t.TempDir(), CWD: t.TempDir(), Host: "host", Platform: "test", PID: 123, Now: func() time.Time { return f.now }, Probe: func(context.Context, int) (Process, error) { return f.process, nil }}
	if harness == "claude" {
		f.config.Format = ClaudeSessionV1
		f.path = filepath.Join(f.config.Home, "sessions", "123.json")
	} else {
		f.config.Format = CodexRolloutV1
		f.path = filepath.Join(f.config.Home, "sessions", "2026", "10", "10", "rollout-2026-10-10T06-00-00-"+sessionA+".jsonl")
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0700); err != nil {
		t.Fatal(err)
	}
	f.observer = New(f.config)
	return f
}
func (f *fixture) write(data string) {
	f.t.Helper()
	if err := os.WriteFile(f.path, []byte(data), 0600); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) append(data string) {
	f.t.Helper()
	file, err := os.OpenFile(f.path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		f.t.Fatal(err)
	}
	defer file.Close()
	if _, err = file.WriteString(data); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) sample() Observation {
	return f.observer.Observe(context.Background(), f.config.Binding)
}
func (f *fixture) claude(status, session string) string {
	b, _ := json.Marshal(map[string]any{"pid": 123, "sessionId": session, "cwd": f.config.CWD, "kind": "interactive", "pidDomain": "test:host", "procStart": "123456", "status": status, "statusUpdatedAt": f.now.UnixMilli()})
	return string(b)
}
func (f *fixture) meta(session string) string {
	b, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": session, "cwd": f.config.CWD}})
	return string(b) + "\n"
}
func (f *fixture) event(kind, turn string) string {
	b, _ := json.Marshal(map[string]any{"type": "event_msg", "timestamp": f.now.Format(time.RFC3339Nano), "payload": map[string]any{"type": kind, "turn_id": turn}})
	return string(b) + "\n"
}

func TestConversationActivityStartEndAndInvalidEvidence(t *testing.T) {
	for _, h := range []string{"claude", "codex"} {
		t.Run(h, func(t *testing.T) {
			f := newFixture(t, h)
			if h == "claude" {
				f.write(f.claude("busy", sessionA))
			} else {
				f.write(f.meta(sessionA) + f.event("task_started", "turn-a"))
			}
			first := f.sample()
			if first.State != Working {
				t.Errorf("start state=%s, want working", first.State)
			}
			f.now = f.now.Add(time.Second)
			if h == "claude" {
				f.write(f.claude("idle", sessionA))
			} else {
				f.append(f.event("task_complete", "turn-a"))
			}
			second := f.sample()
			if second.State != Idle {
				t.Errorf("end state=%s, want idle", second.State)
			}
			if second.Sequence != 2 || !second.ObservedAt.Equal(f.now) {
				t.Error("sample sequence/time missing")
			}
			f.write("{broken")
			if got := f.sample(); got.State != Unknown {
				t.Errorf("corrupt input state=%s", got.State)
			}
			mismatch := newFixture(t, h)
			if h == "claude" {
				mismatch.write(mismatch.claude("busy", sessionB))
			} else {
				mismatch.write(mismatch.meta(sessionB) + mismatch.event("task_started", "turn-a"))
			}
			if got := mismatch.sample(); got.State != Unknown {
				t.Errorf("identity mismatch state=%s", got.State)
			}
		})
	}
}
