package channel

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/nativeinbox"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestClaudeInheritedInboxFencesProcessAndCurrentConversation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix inbox evidence")
	}
	const initialSID = "22222222-2222-4222-8222-222222222222"
	const resumedSID = "11111111-1111-4111-8111-111111111111"
	dir, err := os.MkdirTemp("", "p1-inbox-")
	if err != nil {
		t.Fatal("inbox directory")
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "i.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal("inbox listen")
	}
	defer listener.Close()
	t.Setenv("CLAUDE_CODE_SESSION_ID", initialSID)
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "synthetic-only")
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", path)
	initial := broker.AttachRequest{Harness: "claude", SessionID: initialSID, PID: os.Getpid(), CWD: dir}
	var mu sync.Mutex
	current, key := initial, "process-start-one"
	observe := func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) {
		mu.Lock()
		defer mu.Unlock()
		return current, nil
	}
	probe := func(context.Context, int) (string, error) { mu.Lock(); defer mu.Unlock(); return key, nil }
	factory := newClaudeInboxFactory(t.Context(), initial, observe, probe)
	mu.Lock()
	current.SessionID = resumedSID
	resumed := current
	mu.Unlock()
	sink, _, err := factory(resumed)
	if err != nil {
		t.Fatal("same-parent resume rejected inherited inbox")
	}
	accepted := make(chan bool, 1)
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			accepted <- false
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		decoder := json.NewDecoder(conn)
		var auth struct{ Type, Token string }
		var frame struct {
			Type    string
			Message struct{ Role, Content string }
		}
		ok := decoder.Decode(&auth) == nil && auth.Type == "auth" && auth.Token == "synthetic-only" && decoder.Decode(&frame) == nil && frame.Type == "user" && frame.Message.Role == "user"
		var message broker.DeliveredMessage
		ok = ok && json.Unmarshal([]byte(frame.Message.Content), &message) == nil && message.Body == "selected-fields-only" && message.To == "resumed"
		accepted <- ok
	}()
	if err := sink(t.Context(), broker.Message{From: "peer", To: "resumed", Body: "selected-fields-only"}); err != nil {
		t.Fatal("inherited inbox write failed")
	}
	select {
	case ok := <-accepted:
		if !ok {
			t.Fatal("native auth/user frame mismatch")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("native frame missing")
	}
	for _, kind := range []string{"pid-reused", "other-pid", "other-cwd", "other-session", "env-session", "env-socket", "env-token"} {
		t.Run(kind, func(t *testing.T) {
			mu.Lock()
			current, key = resumed, "process-start-one"
			switch kind {
			case "pid-reused":
				key = "process-start-two"
			case "other-pid":
				current.PID++
			case "other-cwd":
				current.CWD = dir + "-other"
			case "other-session":
				current.SessionID = initialSID
			}
			mu.Unlock()
			switch kind {
			case "env-session":
				t.Setenv("CLAUDE_CODE_SESSION_ID", resumedSID)
			case "env-socket":
				t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", path+"-stale")
			case "env-token":
				t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "replaced-synthetic")
			}
			if _, _, err := factory(resumed); !errors.Is(err, nativeinbox.ErrUnavailable) {
				t.Fatal("factory crossed process/conversation boundary")
			}
			if err := sink(t.Context(), broker.Message{From: "peer", To: "resumed", Body: "must-not-deliver"}); !errors.Is(err, nativeinbox.ErrUnavailable) {
				t.Fatal("existing sink crossed process/conversation boundary")
			}
		})
	}
	mu.Lock()
	current, key = resumed, "process-start-one"
	mu.Unlock()
	unproven := initial
	unproven.SessionID = resumedSID
	if _, _, err := newClaudeInboxFactory(t.Context(), unproven, observe, func(context.Context, int) (string, error) { return "", errors.New("unverified parent") })(resumed); !errors.Is(err, nativeinbox.ErrUnavailable) {
		t.Fatal("unproven initial env provenance allowed a mapping")
	}
}
