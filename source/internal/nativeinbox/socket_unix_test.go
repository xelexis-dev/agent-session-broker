//go:build !windows

package nativeinbox

import (
	"bufio"
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

func TestUnixLocalSocketReceivesPost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	testEnvironment(t)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "uds:"+path)
	p, err := FromEnvironment(testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	posted := make(chan error, 1)
	go func() { posted <- p.Post(context.Background(), broker.Message{Body: "unix socket"}) }()
	_ = listener.(*net.UnixListener).SetDeadline(time.Now().Add(time.Second))
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	_ = server.SetReadDeadline(time.Now().Add(time.Second))
	scanner := bufio.NewScanner(server)
	for range 2 {
		if !scanner.Scan() {
			t.Fatal("missing frame")
		}
	}
	if err := <-posted; err != nil {
		t.Fatal(err)
	}
}
