package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	protocol "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestPushExternalWindowsCommandLimit(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows command-line limit")
	}
	a, r, key := externalPushFixture(t)
	a.externalQueue = func(context.Context, string, string, []string) error {
		t.Fatal("oversized command executed")
		return nil
	}
	if err := a.PushExternal(context.Background(), r, key, broker.Message{Body: strings.Repeat("a", 40000)}); err == nil || !strings.Contains(err.Error(), "Windows command-line limit") {
		t.Fatalf("got %v", err)
	}
}

func externalPushFixture(t *testing.T) (*Adapter, broker.AttachRequest, string) {
	t.Helper()
	a := NewAdapter()
	a.externalHome = t.TempDir()
	r := broker.AttachRequest{Harness: "codex", Name: "receiver", SessionID: "01a070ed-0c7f-71b0-a10e-885c3a8e89b8", PID: 123, CWD: t.TempDir()}
	dir := filepath.Join(a.externalHome, "sessions", "2026", "09", "05")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": r.SessionID, "cwd": r.CWD}})
	if err := os.WriteFile(filepath.Join(dir, "rollout-date-"+r.SessionID+".jsonl"), append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	key := "123:456:" + filepath.Join(t.TempDir(), "codex")
	a.externalProbe = func(_ context.Context, pid int) (string, error) {
		if pid != r.PID {
			t.Fatalf("wrong PID %d", pid)
		}
		return key, nil
	}
	return a, r, key
}

func TestPushExternalPreservesEnvelopeAndPinnedCommand(t *testing.T) {
	a, r, key := externalPushFixture(t)
	m := broker.Message{FromRegistrationID: strings.Repeat("A", 26), SentAt: time.Date(2026, 10, 18, 1, 2, 3, 456000000, time.UTC), From: "sender", To: "receiver", Body: "안녕\n\"quoted\"; $(secret) `cmd` & --thread other"}
	calls := 0
	a.externalQueue = func(ctx context.Context, executable, cwd string, args []string) error {
		calls++
		if executable != strings.SplitN(key, ":", 3)[2] || cwd != r.CWD {
			t.Fatal("unpinned command")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 10*time.Second {
			t.Fatal("unbounded command")
		}
		if len(args) != 5 || !reflect.DeepEqual(args[:4], []string{"queue", "--thread", r.SessionID, "--message"}) {
			t.Fatalf("wrong arguments: %v", args[:min(len(args), 4)])
		}
		var got broker.DeliveredMessage
		if err := json.Unmarshal([]byte(args[4]), &got); err != nil {
			t.Fatal(err)
		}
		if got.FromRegistrationID != m.FromRegistrationID || got.From != m.From || got.To != m.To || got.Body != m.Body || !got.SentAt.Equal(m.SentAt) {
			t.Fatal("envelope changed")
		}
		return nil
	}
	if err := a.PushExternal(context.Background(), r, key, m); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestPushExternalAcceptsDarwinProcessKey(t *testing.T) {
	a, r, _ := externalPushFixture(t)
	executable := filepath.Join(t.TempDir(), "Codex App", "codex")
	if runtime.GOOS != "windows" {
		// A colon is legal in a macOS executable path and is not a key field.
		executable = filepath.Join(t.TempDir(), "Codex: App", "codex")
	}
	key := "darwin:1789000000:123456:" + executable
	a.externalProbe = func(context.Context, int) (string, error) { return key, nil }
	if _, observed, err := a.InspectExternal(context.Background(), r); err != nil || observed != key {
		t.Fatalf("registration inspection failed: %v", err)
	}
	calls := 0
	a.externalQueue = func(_ context.Context, got, cwd string, args []string) error {
		calls++
		if got != executable || cwd != r.CWD || len(args) != 5 || !reflect.DeepEqual(args[:4], []string{"queue", "--thread", r.SessionID, "--message"}) {
			t.Fatal("queue did not use the observed executable, cwd and thread")
		}
		return nil
	}
	if err := a.PushExternal(context.Background(), r, key, broker.Message{From: "sender", To: r.Name, Body: "first message"}); err != nil {
		t.Fatalf("registered Darwin identity could not receive its first message: %v", err)
	}
	if calls != 1 {
		t.Fatalf("queue calls = %d", calls)
	}
	// Compare the entire key, including microseconds, before extracting a path.
	if err := a.PushExternal(context.Background(), r, "darwin:1789000000:123457:"+executable, broker.Message{Body: "stale"}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("changed Darwin process generation accepted: %v", err)
	}
	if calls != 1 {
		t.Fatal("stale identity invoked queue")
	}
}

func TestPushExternalRejectsStaleOrInvalidIdentity(t *testing.T) {
	for _, kind := range []string{"changed-key", "empty-key", "wrong-cwd", "wrong-id", "dead", "malformed-key"} {
		t.Run(kind, func(t *testing.T) {
			a, r, key := externalPushFixture(t)
			switch kind {
			case "changed-key":
				key = "different:" + key
			case "empty-key":
				key = ""
			case "wrong-cwd":
				r.CWD = t.TempDir()
			case "wrong-id":
				r.SessionID = "01a070ed-0c7f-71b0-a10e-885c3a8e89b9"
			case "dead":
				a.externalProbe = func(context.Context, int) (string, error) { return "", ErrSessionNotFound }
			case "malformed-key":
				key = "123:456:relative"
				a.externalProbe = func(context.Context, int) (string, error) { return key, nil }
			}
			a.externalQueue = func(context.Context, string, string, []string) error {
				t.Fatal("executed rejected request")
				return nil
			}
			if err := a.PushExternal(context.Background(), r, key, broker.Message{}); !errors.Is(err, ErrSessionNotFound) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestPushExternalSanitizesFailureAndHonorsCancellation(t *testing.T) {
	a, r, key := externalPushFixture(t)
	a.externalQueue = func(context.Context, string, string, []string) error {
		return errors.New("private CLI output and message")
	}
	err := a.PushExternal(context.Background(), r, key, broker.Message{Body: "private"})
	if err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsanitized failure: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.externalQueue = func(context.Context, string, string, []string) error {
		t.Fatal("executed canceled request")
		return nil
	}
	if err := a.PushExternal(ctx, r, key, broker.Message{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	a.externalQueue = func(ctx context.Context, _ string, _ string, _ []string) error { <-ctx.Done(); return ctx.Err() }
	if err := a.PushExternal(ctx, r, key, broker.Message{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestAttachmentExternalProjection(t *testing.T) {
	a, r, key := externalPushFixture(t)
	a.externalQueue = func(_ context.Context, _, _ string, args []string) error {
		var got broker.DeliveredMessage
		if json.Unmarshal([]byte(args[4]), &got) != nil || got.Attachment == nil {
			t.Fatal("local attachment lost")
		}
		if got.Attachment.Path != "/inbox/synthetic.bin" || got.Attachment.ID != "" || got.Attachment.Name != "" || !strings.Contains(got.Body, protocol.AttachmentReminder) {
			t.Fatal("unsafe projection")
		}
		return nil
	}
	if e := a.PushExternal(t.Context(), r, key, broker.Message{Body: "synthetic", Attachment: &protocol.Attachment{ID: "hidden-id", Name: "hidden-name", Path: "/inbox/synthetic.bin", Size: 7, SHA256: strings.Repeat("a", 64)}}); e != nil {
		t.Fatal(e)
	}
}
