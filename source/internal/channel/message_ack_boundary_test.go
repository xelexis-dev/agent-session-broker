package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

const ackKimiSession = "session_11111111-1111-4111-8111-111111111111"

func messageAckFixture(t *testing.T, store *clientstate.Store, epoch, lease string) (*messageAckState, *ingressBinding, durableOptions) {
	t.Helper()
	if store == nil {
		var err error
		store, err = clientstate.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
	}
	opts := (durableOptions{state: store, scope: "scope"}).withDefaults()
	b := &ingressBinding{mu: &sync.Mutex{}, identity: broker.AttachRequest{ChannelID: lease}}
	result := broker.RegisterResult{MessageAckVersion: 1, Session: broker.Session{RegistrationID: "AAAAAAAAAAAAAAAAAAAAAAAAAA", Epoch: epoch}}
	if err := opts.negotiateMessageAck(b, result, lease, []string{broker.MessageAckCapability}); err != nil {
		t.Fatal(err)
	}
	return bindingMessageAck(b), b, opts
}
func TestMessageAckCompletedPrefixSurvivesLeaseAndProcess(t *testing.T) {
	state, b, opts := messageAckFixture(t, nil, "epoch", "lease")
	for _, seq := range []uint64{1, 2} {
		if _, err := state.observe(seq); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.complete(2); err != nil {
		t.Fatal(err)
	}
	if got, err := opts.state.LoadDeliveryWatermark(opts.scope, state.registration, state.epoch); err != nil || got != 0 {
		t.Fatal("max sequence acknowledged unfinished prefix")
	}
	if err := state.complete(1); err != nil {
		t.Fatal(err)
	}
	result := broker.RegisterResult{MessageAckVersion: 1, Session: broker.Session{RegistrationID: state.registration, Epoch: state.epoch}}
	if err := opts.negotiateMessageAck(b, result, "new-lease", []string{broker.MessageAckCapability}); err != nil {
		t.Fatal(err)
	}
	same := bindingMessageAck(b)
	if skip, err := same.observe(1); err != nil || !skip {
		t.Fatal("lease change forgot completed watermark")
	}
	if through, cursor := same.snapshot(); through != 1 || cursor != 1 {
		t.Fatal("persisted watermark sent a future ack on new lease")
	}
	reopened, err := clientstate.Open(opts.state.Dir())
	if err != nil {
		t.Fatal(err)
	}
	restarted, _, _ := messageAckFixture(t, reopened, "epoch", "process-lease")
	if skip, err := restarted.observe(2); err != nil || !skip {
		t.Fatal("process restart forgot watermark")
	}
	changed, _, _ := messageAckFixture(t, reopened, "new-epoch", "other-lease")
	if skip, err := changed.observe(1); err != nil || skip {
		t.Fatal("watermark leaked across server epochs")
	}
}

type ackWriteProbe struct {
	mcp.Connection
	write func(context.Context, jsonrpc.Message) error
}

func (p ackWriteProbe) Write(ctx context.Context, m jsonrpc.Message) error { return p.write(ctx, m) }
func TestMessageAckCodexBusyCompletesOnlyAfterHookWrite(t *testing.T) {
	for _, mode := range []string{"success", "write-failed", "closed"} {
		t.Run(mode, func(t *testing.T) {
			state, b, _ := messageAckFixture(t, nil, "epoch", "lease")
			router := newCodexIngress(func(context.Context, broker.Message) error { return errors.New("busy message unexpectedly pushed") })
			if _, err := writeCodexHook(router, "UserPromptSubmit"); err != nil {
				t.Fatal(err)
			}
			if _, err := state.observe(1); err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(t.Context(), handoffKey{}, state.completion(t.Context(), b, 1))
			if err := router.deliver(ctx, broker.Message{DeliverySequence: 1, Body: "m1"}); err != nil {
				t.Fatal(err)
			}
			if through, _ := state.snapshot(); through != 0 {
				t.Fatal("pending enqueue acknowledged host handoff")
			}
			out, done, failed, err := router.hookHandoff("PostToolUse")
			if err != nil || done == nil {
				t.Fatal("hook did not collect completion")
			}
			writes := &hookWriteCompletions{}
			result := writes.result(done, failed)
			result.StructuredContent = out
			raw, _ := json.Marshal(result)
			entered, release := make(chan struct{}), make(chan struct{})
			conn := &hookWriteConnection{completions: writes, Connection: ackWriteProbe{write: func(_ context.Context, message jsonrpc.Message) error {
				encoded, _ := jsonrpc.EncodeMessage(message)
				if bytes.Contains(encoded, []byte(hookWriteKey)) {
					t.Error("private handoff marker leaked to host")
				}
				close(entered)
				<-release
				if mode == "write-failed" {
					return io.ErrClosedPipe
				}
				return nil
			}}}
			written := make(chan error, 1)
			go func() { written <- conn.Write(t.Context(), &jsonrpc.Response{Result: raw}) }()
			<-entered
			if through, _ := state.snapshot(); through != 0 {
				t.Fatal("hook acknowledged before Write returned")
			}
			if mode == "closed" {
				router.close()
			}
			close(release)
			err = <-written
			if mode == "write-failed" && err == nil {
				t.Fatal("write error swallowed")
			}
			through, _ := state.snapshot()
			if through != map[bool]uint64{true: 1, false: 0}[mode == "success"] {
				t.Fatal("wrong host handoff watermark", mode, through)
			}
		})
	}
}
func TestMessageAckCodexIdleAndFencedCompletion(t *testing.T) {
	for _, mode := range []string{"success", "queue-failed", "fenced", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			state, b, _ := messageAckFixture(t, nil, "epoch", "lease")
			_, _ = state.observe(1)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			callback := state.completion(ctx, b, 1)
			router := newCodexIngress(func(context.Context, broker.Message) error {
				if mode == "queue-failed" {
					return errors.New("queue failed")
				}
				return nil
			})
			if mode == "fenced" {
				b.identity.ChannelID = "another-lease"
			}
			if mode == "cancelled" {
				cancel()
			}
			_ = router.deliver(context.WithValue(ctx, handoffKey{}, callback), broker.Message{DeliverySequence: 1, Body: "m1"})
			through, _ := state.snapshot()
			if through != map[bool]uint64{true: 1, false: 0}[mode == "success"] {
				t.Fatal("idle/fenced ack mismatch")
			}
		})
	}
}
func TestMessageAckKimiRequiresV2ReceiptAndRetriesFailure(t *testing.T) {
	state, b, _ := messageAckFixture(t, nil, "epoch", "lease")
	_, _ = state.observe(1)
	cwd := t.TempDir()
	k := newKimiClient(os.Getpid(), cwd)
	if _, err := k.accept(kimiHookInput{Version: 2, Event: "SessionStart", SessionID: ackKimiSession, CWD: cwd}); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(t.Context(), handoffKey{}, state.completion(t.Context(), b, 1))
	if err := k.queue.deliver(ctx, broker.Message{DeliverySequence: 1, Body: "m1"}); err != nil {
		t.Fatal(err)
	}
	old, err := k.accept(kimiHookInput{Event: "UserPromptSubmit", SessionID: ackKimiSession, CWD: cwd})
	if err != nil || len(old) != 0 || k.queue.pendingCount() != 1 {
		t.Fatal("old helper drained an ack lease")
	}
	packet, err := k.accept(kimiHookInput{Version: 2, Event: "UserPromptSubmit", SessionID: ackKimiSession, CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	receipt, _ := packet["receipt"].(string)
	if receipt == "" {
		t.Fatal("missing helper receipt")
	}
	if through, _ := state.snapshot(); through != 0 {
		t.Fatal("IPC encode acknowledged stdout")
	}
	k.retryReceipt(receipt)
	if k.queue.pendingCount() != 1 {
		t.Fatal("failed stdout/receipt did not retain pending messages")
	}
	packet, err = k.accept(kimiHookInput{Version: 2, Event: "UserPromptSubmit", SessionID: ackKimiSession, CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	receipt, _ = packet["receipt"].(string)
	if _, err := k.accept(kimiHookInput{Version: 2, Event: "Receipt", Receipt: receipt, SessionID: ackKimiSession, CWD: cwd}); err != nil {
		t.Fatal(err)
	}
	if through, _ := state.snapshot(); through != 1 {
		t.Fatal("helper v2 receipt did not complete")
	}
}

type ackOutputWriter struct{ write func([]byte) (int, error) }

func (w ackOutputWriter) Write(p []byte) (int, error) { return w.write(p) }
func TestMessageAckKimiHelperWritesBeforeReceipt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Kimi uses Unix IPC")
	}
	for _, short := range []bool{false, true} {
		t.Run(fmt.Sprint(short), func(t *testing.T) {
			dir := shortSocketDir(t, "ack-kimi-")
			path := filepath.Join(dir, "hook.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			var receipts atomic.Int32
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					data, _ := io.ReadAll(conn)
					var input kimiHookInput
					_ = json.Unmarshal(data, &input)
					if input.Event == "Receipt" {
						receipts.Add(1)
					}
					_, _ = conn.Write([]byte("{}\n"))
					_ = conn.Close()
				}
			}()
			packet, _ := json.Marshal(map[string]any{"version": 2, "receipt": "synthetic-receipt", "output": map[string]any{"message": "m1"}})
			writer := ackOutputWriter{write: func(data []byte) (int, error) {
				if receipts.Load() != 0 {
					t.Error("receipt sent before stdout write")
				}
				if short {
					return len(data) - 1, nil
				}
				return len(data), nil
			}}
			err = writeKimiHookResponse(path, kimiHookInput{Version: 2, SessionID: ackKimiSession, CWD: dir}, packet, writer)
			if short {
				if !errors.Is(err, io.ErrShortWrite) || receipts.Load() != 0 {
					t.Fatal("short stdout write acknowledged")
				}
			} else if err != nil || receipts.Load() != 1 {
				t.Fatal("successful stdout write lacked receipt")
			}
			_ = listener.Close()
			<-serverDone
		})
	}
}
func TestMessageAckFollowHeldCompletesAfterPost(t *testing.T) {
	state, b, opts := messageAckFixture(t, nil, "epoch", "lease")
	_, _ = state.observe(1)
	message := broker.Message{DeliverySequence: 1, Body: "held"}
	b.followMessage = &message
	b.followAck = state.completion(t.Context(), b, 1).follow
	entered, release := make(chan struct{}), make(chan struct{})
	target := &ingressBinding{mu: &sync.Mutex{}, sink: func(context.Context, broker.Message) error { close(entered); <-release; return nil }}
	follow := &claudeFollow{opts: &opts}
	finished := make(chan bool, 1)
	go func() { finished <- follow.deliverPending(t.Context(), b, target) }()
	<-entered
	if through, _ := state.snapshot(); through != 0 {
		t.Fatal("held follow message acknowledged before Post")
	}
	close(release)
	if !<-finished {
		t.Fatal("follow Post failed")
	}
	if through, _ := state.snapshot(); through != 1 || b.followMessage != nil {
		t.Fatal("follow completion not recorded")
	}
}

func TestMessageAckActiveLeaseCannotSilentlyDowngrade(t *testing.T) {
	state, b, opts := messageAckFixture(t, nil, "epoch", "lease")
	result := broker.RegisterResult{MessageAckVersion: 1, Session: broker.Session{RegistrationID: state.registration, Epoch: state.epoch}}
	if err := opts.negotiateMessageAck(b, result, state.lease, nil); err != nil || bindingMessageAck(b) != state {
		t.Fatal("capability loss changed an existing lease")
	}
	result.MessageAckVersion = 0
	if err := opts.negotiateMessageAck(b, result, state.lease, nil); err == nil || bindingMessageAck(b) != state {
		t.Fatal("active acknowledgement silently became legacy")
	}
}
