//go:build !windows

package channel

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/nativeinbox"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestRestartInboxUsesVerifiedCurrentParentWithTemporaryEnvironment(t *testing.T) {
	const temporary = "22222222-2222-4222-8222-222222222222"
	const restored = "11111111-1111-4111-8111-111111111111"
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	t.Setenv("CLAUDE_CODE_SESSION_ID", temporary)
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "synthetic-only")
	dir, err := os.MkdirTemp("", "restart-inbox-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	endpoint := filepath.Join(dir, "i.sock")
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", endpoint)
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	id := broker.AttachRequest{Harness: "claude", SessionID: restored, PID: os.Getpid(), CWD: root}
	if err = os.MkdirAll(filepath.Join(root, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "sessions", strconv.Itoa(id.PID)+".json")
	data, _ := json.Marshal(map[string]any{"pid": id.PID, "kind": "interactive", "sessionId": restored, "cwd": root})
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	key := func(context.Context, int) (string, error) { return "same-parent-generation", nil }
	observe := func(ctx context.Context, _ broker.AttachRequest) (broker.AttachRequest, error) {
		return claudeParentSessionFile(ctx, id.PID, readClaudeSessionFile, key)
	}
	factory := newClaudeInboxFactory(t.Context(), id, observe, key)
	sink, _, err := factory(id)
	if err != nil {
		t.Fatal("restart refused current parent because inherited session is temporary")
	}
	got := make(chan bool, 1)
	go func() {
		c, e := listener.Accept()
		if e != nil {
			got <- false
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		d := json.NewDecoder(c)
		var auth struct{ Type, Token string }
		var frame struct {
			Type    string
			Message struct{ Content string }
		}
		ok := d.Decode(&auth) == nil && auth.Type == "auth" && auth.Token == "synthetic-only" && d.Decode(&frame) == nil && frame.Type == "user"
		var message broker.DeliveredMessage
		ok = ok && json.Unmarshal([]byte(frame.Message.Content), &message) == nil && message.Body == "once"
		var extra any
		ok = ok && d.Decode(&extra) != nil
		got <- ok
	}()
	if err = sink(t.Context(), broker.Message{From: "peer", To: "restored", Body: "once"}); err != nil {
		t.Fatal("current parent inbox delivery refused", err)
	}
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("expected one authenticated message")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no message")
	}
	data, _ = json.Marshal(map[string]any{"pid": id.PID, "kind": "interactive", "sessionId": temporary, "cwd": root})
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = sink(t.Context(), broker.Message{From: "peer", To: "restored", Body: "stale"}); !errors.Is(err, nativeinbox.ErrUnavailable) {
		t.Fatal("stale conversation accepted")
	}
}
