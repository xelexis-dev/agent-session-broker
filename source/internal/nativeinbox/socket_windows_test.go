package nativeinbox

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"golang.org/x/sys/windows"
)

// This creates only a test-owned local pipe, never a harness or real inbox.
func localTestPipe(t *testing.T) (string, windows.Handle) {
	t.Helper()
	path := fmt.Sprintf(`\\.\pipe\broker-native-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateNamedPipe(name, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_OVERLAPPED, windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS, 1, 1024, 1024, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	return path, handle
}

func acceptTestPipe(handle windows.Handle) (*os.File, error) {
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(event)
	overlapped := windows.Overlapped{HEvent: event}
	err = windows.ConnectNamedPipe(handle, &overlapped)
	if err == windows.ERROR_IO_PENDING {
		status, waitErr := windows.WaitForSingleObject(event, 2000)
		if waitErr != nil || status != windows.WAIT_OBJECT_0 {
			_ = windows.CancelIoEx(handle, &overlapped)
			var transferred uint32
			_ = windows.GetOverlappedResult(handle, &overlapped, &transferred, true)
			return nil, fmt.Errorf("test pipe accept did not complete")
		}
		var transferred uint32
		err = windows.GetOverlappedResult(handle, &overlapped, &transferred, false)
	}
	// A fast client can write and close before this goroutine starts accepting.
	// ERROR_NO_DATA describes that closed client; its bytes remain readable on
	// this single-use server instance. Drain them before closing the handle.
	if err != nil && err != windows.ERROR_PIPE_CONNECTED && err != windows.ERROR_NO_DATA {
		return nil, err
	}
	return os.NewFile(uintptr(handle), "test-native-inbox"), nil
}

func TestWindowsLocalPipeReceivesPost(t *testing.T) {
	path, handle := localTestPipe(t)
	testEnvironment(t)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "uds:"+path)
	p, err := FromEnvironment(testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan error, 1)
	go func() {
		server, err := acceptTestPipe(handle)
		if err != nil {
			windows.CloseHandle(handle)
			received <- err
			return
		}
		defer server.Close()
		_ = server.SetReadDeadline(time.Now().Add(2 * time.Second))
		scanner := bufio.NewScanner(server)
		for _, expected := range []string{`"type":"auth"`, `"type":"user"`} {
			if !scanner.Scan() || !strings.Contains(scanner.Text(), expected) {
				received <- fmt.Errorf("missing expected frame")
				return
			}
		}
		received <- nil
	}()
	if err := p.Post(context.Background(), broker.Message{From: "sender", To: "recipient", Body: "local pipe"}); err != nil {
		t.Fatal(err)
	}
	if err := <-received; err != nil {
		t.Fatal(err)
	}
}

func TestWindowsClientClosePreservesBufferedFrames(t *testing.T) {
	path, handle := localTestPipe(t)
	testEnvironment(t)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "uds:"+path)
	p, err := FromEnvironment(testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	// Force the client to finish and close before the server starts accepting.
	// This is the ordering behind the original intermittent accept failure.
	if err := p.Post(context.Background(), broker.Message{Body: "retained after close"}); err != nil {
		windows.CloseHandle(handle)
		t.Fatal(err)
	}
	server, err := acceptTestPipe(handle)
	if err != nil {
		windows.CloseHandle(handle)
		t.Fatalf("late accept rejected a pipe with buffered frames: %v", err)
	}
	defer server.Close()
	if err := server.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(server)
	for _, expected := range []string{`"type":"auth"`, `"type":"user"`} {
		if !scanner.Scan() || !strings.Contains(scanner.Text(), expected) {
			t.Fatalf("client close lost buffered frame: %v", scanner.Err())
		}
	}
}

func TestWindowsLocalPipeBlockedWriteAndBusyDialAreBounded(t *testing.T) {
	path, handle := localTestPipe(t)
	defer windows.CloseHandle(handle)
	testEnvironment(t)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "uds:"+path)
	p, err := FromEnvironment(testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	p.timeout = 50 * time.Millisecond
	start := time.Now()
	if err := p.Post(context.Background(), broker.Message{Body: strings.Repeat("x", 32*1024)}); err == nil {
		t.Fatal("unread pipe accepted unbounded frame")
	}
	if time.Since(start) > time.Second {
		t.Fatal("blocked pipe write exceeded bound")
	}

	// The undisconnected single server instance remains busy after client close.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start = time.Now()
	conn, err := dialLocal(ctx, path)
	if err == nil {
		conn.Close()
		t.Fatal("busy pipe unexpectedly opened")
	}
	if time.Since(start) > time.Second {
		t.Fatal("busy dial exceeded bound")
	}
}
