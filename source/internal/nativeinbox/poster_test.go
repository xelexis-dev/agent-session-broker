package nativeinbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	protocol "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

const testSessionID = "11111111-1111-4111-8111-111111111111"

func testEnvironment(t *testing.T) {
	t.Helper()
	socket := "uds:/tmp/native-inbox-test.sock"
	if runtime.GOOS == "windows" {
		socket = `uds:\\.\pipe\native-inbox-test`
	}
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", socket)
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "fake-secret-token")
	t.Setenv("CLAUDE_CODE_SESSION_ID", testSessionID)
}

func TestFromEnvironmentRequiresExactInheritedIdentity(t *testing.T) {
	for _, name := range []string{"CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_CODE_SESSION_ID"} {
		t.Run(name, func(t *testing.T) {
			testEnvironment(t)
			t.Setenv(name, "")
			if p, err := FromEnvironment(testSessionID); err == nil || p != nil {
				t.Fatal("missing environment accepted")
			}
		})
	}
	for _, id := range []string{"", "invalid", "22222222-2222-4222-8222-222222222222"} {
		t.Run(id, func(t *testing.T) {
			testEnvironment(t)
			if p, err := FromEnvironment(id); err == nil || p != nil {
				t.Fatal("incorrect identity accepted")
			}
		})
	}
	testEnvironment(t)
	if _, err := FromEnvironment(testSessionID); err != nil {
		t.Fatal(err)
	}
}

func TestFromEnvironmentRejectsNonlocalSocket(t *testing.T) {
	for _, socket := range []string{"tcp:127.0.0.1:42", "uds:relative", "uds:", "uds:/tmp/a\x00b", `uds:\\remote\pipe\inbox`, `uds:\\.\pipe\..\file`, `\\remote\pipe\inbox`, `\\.\pipe\LOCAL\..\file`} {
		t.Run(socket, func(t *testing.T) {
			testEnvironment(t)
			if _, err := socketPath(socket); err == nil {
				t.Fatal("unsafe socket accepted")
			}
		})
	}
}

func TestFromEnvironmentAcceptsRawLocalEndpoint(t *testing.T) {
	testEnvironment(t)
	raw := "/tmp/native-inbox-test.sock"
	if runtime.GOOS == "windows" {
		raw = `\\.\pipe\native-inbox-test`
	}
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", raw)
	if _, err := FromEnvironment(testSessionID); err != nil {
		t.Fatalf("raw inherited local endpoint rejected: %v", err)
	}
	if runtime.GOOS == "windows" {
		for _, endpoint := range []string{`\\.\pipe\LOCAL\cc-msg-test`, `uds:\\.\pipe\LOCAL\cc-msg-test`} {
			t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", endpoint)
			if _, err := FromEnvironment(testSessionID); err != nil {
				t.Fatalf("local namespace rejected: %v", err)
			}
		}
	}
}

func testPoster(t *testing.T) *Poster {
	t.Helper()
	testEnvironment(t)
	p, err := FromEnvironment(testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPostWritesAuthAndEscapedEnvelopeInFIFOOrder(t *testing.T) {
	p := testPoster(t)
	client, server := net.Pipe()
	defer server.Close()
	p.dial = func(context.Context, string) (writeConnection, error) { return client, nil }
	messages := []broker.DeliveredMessage{{FromRegistrationID: strings.Repeat("A", 26), SentAt: time.Date(2026, 10, 18, 1, 2, 3, 456000000, time.UTC), From: "sender\"\n", To: "recipient", Body: "한글\n\"quoted\"\\body"}, {SentAt: time.Date(2026, 10, 18, 1, 2, 4, 456000000, time.UTC), From: "next", To: "recipient", Body: "second"}}
	done := make(chan error, 1)
	go func() {
		done <- p.Post(context.Background(), broker.Message{Batch: &broker.MessageBatch{Messages: messages}})
	}()
	scanner := bufio.NewScanner(server)
	if !scanner.Scan() {
		t.Fatal("missing auth")
	}
	var auth map[string]string
	if err := json.Unmarshal(scanner.Bytes(), &auth); err != nil {
		t.Fatal(err)
	}
	if len(auth) != 2 || auth["type"] != "auth" || auth["token"] != "fake-secret-token" {
		t.Fatal("invalid authentication frame")
	}
	previousUUID := ""
	for _, expected := range messages {
		if !scanner.Scan() {
			t.Fatal("missing user frame")
		}
		var frame struct {
			Type, From, UUID, Priority string
			Message                    struct{ Role, Content string }
		}
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Type != "user" || frame.From != "x-broker" || frame.Priority != "next" || frame.Message.Role != "user" || !canonicalUUID(frame.UUID) || frame.UUID == previousUUID {
			t.Fatal("invalid user frame")
		}
		previousUUID = frame.UUID
		var envelope broker.DeliveredMessage
		if err := json.Unmarshal([]byte(frame.Message.Content), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope != expected {
			t.Fatalf("envelope = %#v, want %#v", envelope, expected)
		}
		if strings.Contains(string(scanner.Bytes()), "session_id") {
			t.Fatal("invented session field")
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPostRejectsOversizeAndInvalidBatchesBeforeDial(t *testing.T) {
	for _, message := range []broker.Message{
		{Body: strings.Repeat("x", 64*1024)},
		{Body: strings.Repeat("\x00", 16*1024)},
		{Batch: &broker.MessageBatch{}},
		{Body: "ambiguous", Batch: &broker.MessageBatch{Messages: []broker.DeliveredMessage{{Body: "batch"}}}},
		{Batch: &broker.MessageBatch{Messages: make([]broker.DeliveredMessage, 33)}},
	} {
		p := testPoster(t)
		p.dial = func(context.Context, string) (writeConnection, error) {
			t.Fatal("dialed before validating all frames")
			return nil, nil
		}
		if err := p.Post(context.Background(), message); err == nil {
			t.Fatal("invalid message accepted")
		}
	}
}

func TestPostCancellationAndTimeoutInterruptBlockedWrite(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		p := testPoster(t)
		p.timeout = 40 * time.Millisecond
		client, server := net.Pipe()
		defer server.Close()
		p.dial = func(context.Context, string) (writeConnection, error) { return client, nil }
		ctx, cancel := context.WithCancel(context.Background())
		if cancelEarly {
			time.AfterFunc(10*time.Millisecond, cancel)
		}
		start := time.Now()
		err := p.Post(ctx, broker.Message{Body: "hello"})
		cancel()
		if err == nil || time.Since(start) > time.Second {
			t.Fatalf("write not bounded: %v", err)
		}
		if cancelEarly && !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	}
}

func TestPostRedactsDialAndWriteFailures(t *testing.T) {
	for _, write := range []bool{false, true} {
		p := testPoster(t)
		secretErr := errors.New("fake-secret-token private-socket-path message-body")
		p.dial = func(context.Context, string) (writeConnection, error) {
			if write {
				return failingConnection{err: secretErr}, nil
			}
			return nil, secretErr
		}
		err := p.Post(context.Background(), broker.Message{Body: "message-body"})
		if err == nil {
			t.Fatal("missing failure")
		}
		for _, secret := range []string{"fake-secret-token", "private-socket-path", "message-body"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatal("failure leaks private value")
			}
		}
	}
}

func TestPostAlreadyCanceledDoesNotDial(t *testing.T) {
	p := testPoster(t)
	p.dial = func(context.Context, string) (writeConnection, error) {
		t.Fatal("canceled post dialed")
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Post(ctx, broker.Message{Body: "hello"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

type failingConnection struct{ err error }

func (c failingConnection) Write([]byte) (int, error)        { return 0, c.err }
func (c failingConnection) Close() error                     { return nil }
func (c failingConnection) SetWriteDeadline(time.Time) error { return nil }

var _ io.WriteCloser = failingConnection{}

func TestSinglePostIncludesSentAt(t *testing.T) {
	stamp := time.Date(2026, 10, 18, 1, 2, 3, 456000000, time.UTC)
	frames, err := encodeFrames(broker.Message{From: "a", To: "b", Body: "fixture", SentAt: stamp}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || !strings.Contains(string(frames[0]), `\"sentAt\":\"2026-10-18T01:02:03.456Z\"`) {
		t.Fatal("native inbox omitted sentAt")
	}
}

func TestNativeSingleFramePreservesSourceRegistration(t *testing.T) {
	id := strings.Repeat("A", 26)
	frames, err := encodeFrames(broker.Message{From: "sender", To: "receiver", Body: "single", FromRegistrationID: id}, false)
	if err != nil || len(frames) != 1 {
		t.Fatal("single frame rejected")
	}
	var frame struct{ Message struct{ Content string } }
	if json.Unmarshal(frames[0], &frame) != nil {
		t.Fatal("frame decode")
	}
	var got broker.DeliveredMessage
	if json.Unmarshal([]byte(frame.Message.Content), &got) != nil || got.FromRegistrationID != id {
		t.Fatal("native single frame dropped source")
	}
	if _, err := encodeFrames(broker.Message{Body: "bounded", FromRegistrationID: strings.Repeat("A", maxFrameBytes+1)}, false); err == nil {
		t.Fatal("unbounded source metadata accepted")
	}
}

func TestFromParentEnvironmentRequiresKnownProcess(t *testing.T) {
	testEnvironment(t)
	if _, err := FromParentEnvironment(1, nil); err == nil {
		t.Fatal("missing parent verifier accepted")
	}
	for _, pid := range []int{0, -1} {
		if p, err := FromParentEnvironment(pid, func(context.Context) bool { return true }); err == nil || p != nil {
			t.Fatal("unknown parent accepted")
		}
	}
}

func TestAttachmentNativeFrames(t *testing.T) {
	for _, batch := range []bool{false, true} {
		a := &protocol.Attachment{ID: "hidden-id", Name: "hidden-name", Path: "/inbox/synthetic.bin", Size: 7, SHA256: strings.Repeat("a", 64)}
		message := broker.Message{From: "sender", To: "receiver", Body: "synthetic", Attachment: a}
		if batch {
			message = broker.Message{Batch: &broker.MessageBatch{Messages: []broker.DeliveredMessage{{From: "sender", To: "receiver", Body: "synthetic", Attachment: a}}}}
		}
		frames, e := encodeFrames(message, false)
		if e != nil || len(frames) != 1 {
			t.Fatal("attachment frame rejected")
		}
		text := string(frames[0])
		for _, hidden := range []string{"hidden-id", "hidden-name"} {
			if strings.Contains(text, hidden) {
				t.Fatal("wire reference exposed")
			}
		}
		for _, want := range []string{"attachment", "synthetic.bin", "sha256", protocol.AttachmentReminder} {
			if !strings.Contains(text, want) {
				t.Fatal("projection missing")
			}
		}
	}
}
