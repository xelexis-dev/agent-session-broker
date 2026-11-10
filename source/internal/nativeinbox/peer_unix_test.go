//go:build darwin || linux

package nativeinbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestParentInboxRejectsWrongServerBeforeAuth(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			dir, err := os.MkdirTemp("", "inbox-peer-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			path := filepath.Join(dir, "i.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			t.Setenv("CLAUDE_CODE_SESSION_ID", testSessionID)
			t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", path)
			t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "synthetic-only")
			parentPID := os.Getpid() + 1
			if changed {
				parentPID = os.Getpid()
			}
			poster, err := FromParentEnvironment(parentPID, func(context.Context) bool { return !changed })
			if err != nil {
				t.Fatal(err)
			}
			bytes := make(chan int, 1)
			go func() {
				c, e := listener.Accept()
				if e != nil {
					bytes <- -1
					return
				}
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(time.Second))
				data, _ := io.ReadAll(c)
				bytes <- len(data)
			}()
			if err = poster.Post(t.Context(), broker.Message{From: "a", To: "b", Body: "not-for-this-server"}); !errors.Is(err, ErrUnavailable) {
				t.Fatal("wrong server process accepted")
			}
			select {
			case n := <-bytes:
				if n != 0 {
					t.Fatal("auth or content written to wrong server")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("wrong server connection did not close")
			}
		})
	}
}
