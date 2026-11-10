package channel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func attachmentResult(t *testing.T, result *mcp.CallToolResult) broker.SendResult {
	t.Helper()
	var out broker.SendResult
	raw, _ := json.Marshal(result.StructuredContent)
	if json.Unmarshal(raw, &out) != nil {
		t.Fatal("invalid result")
	}
	return out
}
func TestAttachmentDeviceClientsTransfer(t *testing.T) {
	runtime := newDetachedRuntime(t, true)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte{0, 255, 0, 91, 92, 254, 1, 2, 3, 250}
	sum := sha256.Sum256(data)
	if os.WriteFile(filepath.Join(root, "synthetic.bin"), data, 0600) != nil {
		t.Fatal("fixture create")
	}
	harness := func(kind string, n int) *detachedHarness {
		cache := t.TempDir()
		id := broker.AttachRequest{Harness: kind, SessionID: strings.Repeat("1", 8) + "-1111-4111-8111-" + strings.Repeat("0", 11) + string(rune('0'+n)), PID: 3100 + n, CWD: root}
		h := runtime.identityHarness(t, t.TempDir(), id, func(o *durableOptions) { o.attachmentCache = cache; o.hookWrites = &hookWriteCompletions{} })
		h.attach(t)
		return h
	}
	sender := harness("claude", 1)
	receiver := harness("codex", 2)
	result, err := sender.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"from": sender.session.Name, "to": receiver.session.Name, "body": "synthetic transfer", "attachment": "synthetic.bin", "expectRegistrationId": receiver.session.RegistrationID}})
	if err != nil {
		t.Fatal("send call failed")
	}
	sent := attachmentResult(t, result)
	if !sent.Accepted {
		t.Fatalf("attachment not accepted: %s", sent.Code)
	}
	select {
	case message := <-receiver.received:
		a := message.Attachment
		if a == nil || a.Error != "" {
			t.Fatalf("attachment projection failed: %+v", a)
		}
		if a.ID != "" || a.Name != "" || a.SHA256 != hex.EncodeToString(sum[:]) || a.Size != int64(len(data)) {
			t.Fatal("wire reference or digest differs")
		}
		received, e := os.ReadFile(a.Path)
		if e != nil || !bytes.Equal(received, data) {
			t.Fatal("received bytes differ")
		}
		if !strings.Contains(message.Body, broker.AttachmentReminder) {
			t.Fatal("untrusted reminder missing")
		}
		encoded, _ := json.Marshal(message)
		if bytes.Contains(encoded, data) {
			t.Fatal("binary in model projection")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("attachment delivery missing")
	}
	waitFor(t, "spool ACK cleanup", func() bool {
		entries, e := os.ReadDir(filepath.Join(runtime.root, "attachments-v1"))
		return e == nil && len(entries) == 0
	})
	// The legacy receiver is ACK-less and must never receive an attachment send.
	old := runtime.harness(t, t.TempDir(), 3133)
	old.attach(t)
	result, err = sender.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"from": sender.session.Name, "to": old.session.Name, "body": "refused", "attachment": "synthetic.bin"}})
	if err != nil || attachmentResult(t, result).Code != "attachment-unsupported" {
		t.Fatal("old receiver accepted attachment")
	}
	select {
	case <-old.received:
		t.Fatal("old receiver got rejected message")
	default:
	}
}
func TestAttachmentExactSendConnectionRecheck(t *testing.T) {
	options := &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{Experimental: map[string]any{broker.AttachmentCapability: true}}}
	capable := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "test"}, options)
	incapable := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "test"}, nil)
	var sends atomic.Int32
	for _, server := range []*mcp.Server{capable, incapable} {
		mcp.AddTool(server, &mcp.Tool{Name: "send_message"}, func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, broker.SendResult, error) {
			sends.Add(1)
			return nil, broker.SendResult{Accepted: true}, nil
		})
	}
	connect := func(server *mcp.Server) *mcp.ClientSession {
		st, ct := mcp.NewInMemoryTransports()
		ss, e := server.Connect(t.Context(), st, nil)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { ss.Close() })
		cs, e := mcp.NewClient(&mcp.Implementation{Name: "fixture", Version: "test"}, nil).Connect(t.Context(), ct, nil)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { cs.Close() })
		return cs
	}
	u := newTestBrokerUpstream(t.Context(), "http://invalid")
	u.current = connect(capable)
	if !u.attachmentSupported(t.Context()) {
		t.Fatal("capable upload session not recognized")
	}
	u.current = connect(incapable)
	result, e := u.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Meta: mcp.Meta{broker.AttachmentMetaKey: strings.Repeat("A", 26)}, Arguments: map[string]any{"from": "a", "to": "b", "body": "body"}})
	if e != nil || attachmentResult(t, result).Code != "unsupported" || sends.Load() != 0 {
		t.Fatal("reconnected old server received send RPC")
	}
}

type attachmentRoundTripper func(*http.Request) (*http.Response, error)

func (f attachmentRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestAttachmentBindingSerializesUploadAndSend(t *testing.T) {
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	data := []byte("synthetic")
	if os.WriteFile(filepath.Join(root, "file"), data, 0600) != nil {
		t.Fatal("fixture")
	}
	{
		server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "test"}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{Experimental: map[string]any{broker.AttachmentCapability: true}}})
		var sends atomic.Int32
		mcp.AddTool(server, &mcp.Tool{Name: "send_message"}, func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, broker.SendResult, error) {
			sends.Add(1)
			return nil, broker.SendResult{Accepted: true}, nil
		})
		st, ct := mcp.NewInMemoryTransports()
		ss, e := server.Connect(t.Context(), st, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer ss.Close()
		cs, e := mcp.NewClient(&mcp.Implementation{Name: "fixture", Version: "test"}, nil).Connect(t.Context(), ct, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer cs.Close()
		u := newTestBrokerUpstream(t.Context(), "https://fixture/mcp?project=fixture")
		u.current = cs
		first := make(chan struct{})
		release := make(chan struct{})
		var posts atomic.Int32
		u.transport = attachmentRoundTripper(func(r *http.Request) (*http.Response, error) {
			if posts.Add(1) == 1 {
				close(first)
			}
			<-release
			raw, e := io.ReadAll(r.Body)
			if e != nil {
				return nil, e
			}
			sum := sha256.Sum256(raw)
			body, _ := json.Marshal(broker.Attachment{ID: strings.Repeat("A", 26), Size: int64(len(raw)), SHA256: hex.EncodeToString(sum[:])})
			return &http.Response{StatusCode: 201, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}, nil
		})
		b := &ingressBinding{mu: &sync.Mutex{}, started: true, identity: broker.AttachRequest{Name: "sender", CWD: root}}
		sender := &attachmentSender{Upstream: u, binding: b}
		var wg sync.WaitGroup
		run := func() {
			defer wg.Done()
			result, e := sender.sendAttachment(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"from": "sender", "to": "receiver", "body": "synthetic", "attachment": "file"}}, "file")
			if e != nil || !attachmentResult(t, result).Accepted {
				t.Error("parallel attachment refused")
			}
		}
		wg.Add(2)
		go run()
		<-first
		go run()
		if b.attachmentMu.TryLock() {
			b.attachmentMu.Unlock()
			t.Error("attachment mutex not held during upload")
		}
		if posts.Load() != 1 || sends.Load() != 0 {
			t.Error("second upload passed binding mutex")
		}
		close(release)
		wg.Wait()
		if posts.Load() != 2 || sends.Load() != 2 {
			t.Fatal("serialized sends incomplete")
		}
	}
}

func attachmentHarness(t *testing.T, r *detachedRuntime, root, state, cache string, number int, created chan *ingressBinding) *detachedHarness {
	t.Helper()
	id := broker.AttachRequest{Harness: "codex", SessionID: fmt.Sprintf("11111111-1111-4111-8111-%012d", number), PID: 3900 + number, CWD: root}
	h := r.identityHarness(t, state, id, func(o *durableOptions) {
		o.hookWrites = &hookWriteCompletions{}
		o.attachmentCache = cache
		if created != nil {
			o.bindingCreated = func(b *ingressBinding) { created <- b }
		}
	})
	h.attach(t)
	return h
}
func TestAttachmentFetchBeforeObserveAndHeartbeat(t *testing.T) {
	r := newDetachedRuntime(t, true)
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if os.WriteFile(filepath.Join(root, "file"), []byte("synthetic"), 0600) != nil {
		t.Fatal("fixture")
	}
	sender := attachmentHarness(t, r, root, t.TempDir(), t.TempDir(), 1, nil)
	state, cache := t.TempDir(), t.TempDir()
	created := make(chan *ingressBinding, 2)
	receiver := attachmentHarness(t, r, root, state, cache, 2, created)
	b := <-created
	entered := make(chan struct{})
	var once sync.Once
	gate := func(w http.ResponseWriter, request *http.Request) bool {
		if request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/attachments/") {
			once.Do(func() { close(entered) })
			<-request.Context().Done()
			return true
		}
		return false
	}
	r.attachmentGate.Store(&gate)
	result, e := sender.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"from": sender.session.Name, "to": receiver.session.Name, "body": "synthetic", "attachment": "file"}})
	if e != nil || !attachmentResult(t, result).Accepted {
		t.Fatal("send")
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("fetch never entered")
	}
	ack := bindingMessageAck(b)
	if ack == nil {
		t.Fatal("ACK not negotiated")
	}
	through, cursor := ack.snapshot()
	if through != 0 || cursor != 0 {
		t.Fatal("download advanced ACK before completion")
	}
	before := r.renews.Load()
	waitFor(t, "heartbeat during download", func() bool { return r.renews.Load() > before })
	receiver.close()
	through, cursor = ack.snapshot()
	if through != 0 || cursor != 0 {
		t.Fatal("cancelled fetch advanced ACK")
	}
	r.attachmentGate.Store(nil)
	resumed := attachmentHarness(t, r, root, state, cache, 2, nil)
	select {
	case message := <-resumed.received:
		if message.Attachment == nil || message.Attachment.Error != "" {
			t.Fatal("redelivery fetch failed")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("failed fetch skipped sequence on resume")
	}
}
func TestAttachmentNewClientOldServer(t *testing.T) {
	binary := os.Getenv("ASB_ATTACHMENT_OLD_SERVER")
	if binary == "" {
		t.Skip("opt-in exact tag0.17.3 server")
	}
	r := newDetachedRuntimeBinary(t, false, binary)
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	a := attachmentHarness(t, r, root, t.TempDir(), t.TempDir(), 4, nil)
	b := attachmentHarness(t, r, root, t.TempDir(), t.TempDir(), 5, nil)
	for _, pair := range [][2]*detachedHarness{{a, b}, {b, a}} {
		result, e := pair[0].client.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"from": pair[0].session.Name, "to": pair[1].session.Name, "body": "synthetic legacy"}})
		if e != nil || !attachmentResult(t, result).Accepted {
			t.Fatal("plain old-server send")
		}
		select {
		case message := <-pair[1].received:
			if message.Body != "synthetic legacy" || message.Attachment != nil {
				t.Fatal("plain legacy bytes changed")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("legacy delivery missing")
		}
	}
	result, e := a.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"from": a.session.Name, "to": b.session.Name, "body": "refused", "attachment": "nonexistent"}})
	if e != nil || attachmentResult(t, result).Code != "unsupported" {
		t.Fatal("old server attachment did not fail before source open")
	}
}

func TestAttachmentTransientDownloadDoesNotACK(t *testing.T) {
	for _, stage := range []string{"headers", "body", "policy-503", "policy-reset", "policy-denied"} {
		t.Run(stage, func(t *testing.T) {
			r := newDetachedRuntime(t, true)
			root, e := filepath.EvalSymlinks(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			data := []byte("synthetic-transient")
			sum := sha256.Sum256(data)
			if os.WriteFile(filepath.Join(root, "file"), data, 0600) != nil {
				t.Fatal("fixture")
			}
			sender := attachmentHarness(t, r, root, t.TempDir(), t.TempDir(), 11, nil)
			created := make(chan *ingressBinding, 1)
			receiver := attachmentHarness(t, r, root, t.TempDir(), t.TempDir(), 12, created)
			binding := <-created
			second := make(chan struct{})
			release := make(chan struct{})
			var count atomic.Int32
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			var downloads atomic.Int32
			if strings.HasPrefix(stage, "policy-") {
				binding.mu.Lock()
				up := attachmentBroker(binding.upstream)
				binding.mu.Unlock()
				up.mu.Lock()
				up.transport = attachmentRoundTripper(func(request *http.Request) (*http.Response, error) {
					if request.URL.Path == "/oauth/transport-policy" {
						attempt := count.Add(1)
						if stage == "policy-denied" {
							return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"project":"detach","allow_http":false}`))}, nil
						}
						if attempt == 1 {
							if stage == "policy-reset" {
								return nil, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
							}
							return &http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
						}
						if attempt == 2 {
							close(second)
							select {
							case <-release:
							case <-request.Context().Done():
								return nil, request.Context().Err()
							}
						}
					}
					if request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/attachments/") {
						downloads.Add(1)
					}
					return http.DefaultTransport.RoundTrip(request)
				})
				up.mu.Unlock()
			}
			if stage == "headers" {
				binding.mu.Lock()
				up := attachmentBroker(binding.upstream)
				binding.mu.Unlock()
				var failHeader atomic.Bool
				up.mu.Lock()
				up.transport = attachmentRoundTripper(func(request *http.Request) (*http.Response, error) {
					if request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/attachments/") && failHeader.CompareAndSwap(false, true) {
						return nil, io.ErrUnexpectedEOF
					}
					return http.DefaultTransport.RoundTrip(request)
				})
				up.mu.Unlock()
			}
			gate := func(w http.ResponseWriter, request *http.Request) bool {
				if strings.HasPrefix(stage, "policy-") {
					return false
				}
				if request.Method != http.MethodGet || !strings.HasPrefix(request.URL.Path, "/attachments/") {
					return false
				}
				attempt := count.Add(1)
				if attempt == 1 {
					if stage == "headers" {
						panic(http.ErrAbortHandler)
					}
					w.Header().Set("Content-Length", strconv.Itoa(len(data)))
					w.Header().Set("X-XBroker-Attachment-SHA256", hex.EncodeToString(sum[:]))
					w.WriteHeader(200)
					w.Write(data[:2])
					w.(http.Flusher).Flush()
					connection, _, e := w.(http.Hijacker).Hijack()
					if e == nil {
						connection.Close()
					}
					return true
				}
				if attempt == 2 {
					close(second)
					select {
					case <-release:
					case <-request.Context().Done():
						return true
					}
				}
				return false
			}
			r.attachmentGate.Store(&gate)
			result, e := sender.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"from": sender.session.Name, "to": receiver.session.Name, "body": "synthetic", "attachment": "file"}})
			if e != nil || !attachmentResult(t, result).Accepted {
				t.Fatal("send")
			}
			if stage == "policy-denied" {
				select {
				case message := <-receiver.received:
					if message.Attachment == nil || message.Attachment.Error != "failed" {
						t.Fatal("permanent policy refusal did not finalize")
					}
				case <-time.After(6 * time.Second):
					t.Fatal("permanent policy refusal retried")
				}
				waitFor(t, "permanent refusal ACK spool cleanup", func() bool {
					files, e := os.ReadDir(filepath.Join(r.root, "attachments-v1"))
					return e == nil && len(files) == 0
				})
				if count.Load() != 1 || downloads.Load() != 0 {
					t.Fatal("permanent policy refusal retried or sent GET")
				}
				return
			}
			select {
			case <-second:
			case message := <-receiver.received:
				_ = message
				t.Fatal("transient GET handed off a final error")
			case <-time.After(6 * time.Second):
				t.Fatal("transient GET not retried")
			}
			ack := bindingMessageAck(binding)
			through, cursor := ack.snapshot()
			if through != 0 || cursor != 0 {
				t.Fatal("transient first attempt advanced ACK")
			}
			select {
			case <-receiver.received:
				t.Fatal("handoff before retry success")
			default:
			}
			once.Do(func() { close(release) })
			select {
			case message := <-receiver.received:
				if message.Attachment == nil || message.Attachment.Error != "" {
					t.Fatal("retry did not deliver file")
				}
				saved, e := os.ReadFile(message.Attachment.Path)
				if e != nil || !bytes.Equal(saved, data) {
					t.Fatal("retry bytes differ")
				}
			case <-time.After(6 * time.Second):
				t.Fatal("retry delivery missing")
			}
			waitFor(t, "retry ACK spool cleanup", func() bool {
				files, e := os.ReadDir(filepath.Join(r.root, "attachments-v1"))
				return e == nil && len(files) == 0
			})
		})
	}
}
func TestAttachmentBusyRenameUsesRegistration(t *testing.T) {
	r := newDetachedRuntime(t, true)
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if os.WriteFile(filepath.Join(root, "file"), []byte("synthetic"), 0600) != nil {
		t.Fatal("fixture")
	}
	sender := attachmentHarness(t, r, root, t.TempDir(), t.TempDir(), 21, nil)
	created := make(chan *ingressBinding, 1)
	receiver := attachmentHarness(t, r, root, t.TempDir(), t.TempDir(), 22, created)
	binding := <-created
	busy := make(chan struct{})
	var calls atomic.Int32
	var badSelector atomic.Bool
	gate := func(w http.ResponseWriter, request *http.Request) bool {
		if request.Method != http.MethodGet || !strings.HasPrefix(request.URL.Path, "/attachments/") {
			return false
		}
		if request.URL.Query().Get("recipient") != broker.SelectorRegistration+receiver.session.RegistrationID {
			badSelector.Store(true)
		}
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(503)
			io.WriteString(w, `{"code":"busy"}`)
			close(busy)
			return true
		}
		return false
	}
	r.attachmentGate.Store(&gate)
	result, e := sender.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"from": sender.session.Name, "to": receiver.session.Name, "body": "synthetic", "attachment": "file"}})
	if e != nil || !attachmentResult(t, result).Accepted {
		t.Fatal("send")
	}
	select {
	case <-busy:
	case <-time.After(5 * time.Second):
		t.Fatal("busy not reached")
	}
	var renamed broker.Session
	if call(t.Context(), receiver.client, "rename_session", map[string]string{"name": "renamed-attachment-recipient"}, &renamed) != nil || renamed.RegistrationID != receiver.session.RegistrationID {
		t.Fatal("rename failed")
	}
	ack := bindingMessageAck(binding)
	through, _ := ack.snapshot()
	if through != 0 {
		t.Fatal("busy advanced ACK before download")
	}
	select {
	case message := <-receiver.received:
		if message.Attachment == nil || message.Attachment.Error != "" {
			t.Fatal("rename broke download")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("renamed download missing")
	}
	if badSelector.Load() || calls.Load() < 2 {
		t.Fatal("GET did not use stable registration selector")
	}
}

func TestAttachmentCrashPartialACKAfterHandoff(t *testing.T) {
	r := newDetachedRuntime(t, true)
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	data := []byte("synthetic-crash-recovery")
	if os.WriteFile(filepath.Join(root, "file"), data, 0600) != nil {
		t.Fatal("fixture")
	}
	sender := attachmentHarness(t, r, root, t.TempDir(), t.TempDir(), 31, nil)
	cache := t.TempDir()
	created := make(chan *ingressBinding, 1)
	handed := make(chan broker.Message, 1)
	release := make(chan struct{})
	var once sync.Once
	id := broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-000000000032", PID: 3932, CWD: root}
	receiver := r.identityHarness(t, t.TempDir(), id, func(o *durableOptions) {
		o.attachmentCache = cache
		o.hookWrites = &hookWriteCompletions{}
		o.bindingCreated = func(b *ingressBinding) { created <- b }
		o.claudeInboxFactory = func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
			return func(ctx context.Context, m broker.Message) error {
				handed <- m
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}, nil, nil
		}
	})
	receiver.attach(t)
	binding := <-created
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var oldPart atomic.Value
	gate := func(w http.ResponseWriter, request *http.Request) bool {
		if request.Method != http.MethodGet || !strings.HasPrefix(request.URL.Path, "/attachments/") {
			return false
		}
		binding.mu.Lock()
		key := binding.durable.key
		binding.mu.Unlock()
		part := filepath.Join(cache, "agent-session-broker", "inbox", key, "."+strings.TrimPrefix(request.URL.Path, "/attachments/")+".part")
		oldPart.Store(part)
		if os.WriteFile(part, []byte("potentially-active-old-attempt"), 0600) != nil {
			t.Error("fresh crash partial fixture")
		}
		return false
	}
	r.attachmentGate.Store(&gate)
	result, e := sender.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"from": sender.session.Name, "to": receiver.session.Name, "body": "synthetic", "attachment": "file"}})
	if e != nil || !attachmentResult(t, result).Accepted {
		t.Fatal("send")
	}
	select {
	case message := <-handed:
		if message.Attachment == nil || message.Attachment.Error != "" {
			t.Fatal("crash partial blocked file")
		}
		saved, e := os.ReadFile(message.Attachment.Path)
		if e != nil || !bytes.Equal(saved, data) {
			t.Fatal("crash recovery bytes differ")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("recovery handoff missing")
	}
	ack := bindingMessageAck(binding)
	through, _ := ack.snapshot()
	if through != 0 {
		t.Fatal("ACK before successful handoff")
	}
	entries, e := os.ReadDir(filepath.Join(r.root, "attachments-v1"))
	if e != nil || len(entries) != 1 {
		t.Fatal("server bytes removed before handoff")
	}
	old, e := os.ReadFile(oldPart.Load().(string))
	if e != nil || string(old) != "potentially-active-old-attempt" {
		t.Fatal("other attempt partial modified")
	}
	once.Do(func() { close(release) })
	waitFor(t, "crash recovery ACK cleanup", func() bool {
		entries, e := os.ReadDir(filepath.Join(r.root, "attachments-v1"))
		return e == nil && len(entries) == 0
	})
}
