package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// The fake host has a live Codex process identity and a queue that writes only
// synthetic messages. The receiver is the unmodified, integrity-checked release.
const ackBinaryHost = `package main
import("encoding/json";"os";"os/exec")
func main(){
 if len(os.Args)>1 && os.Args[1]=="queue" {
  for i,a:=range os.Args {if a=="--message" && i+1<len(os.Args){if os.WriteFile(os.Getenv("ASB_ACK_RECEIPT"),[]byte(os.Args[i+1]),0600)!=nil{os.Exit(2)};return}}
  os.Exit(2)
 }
 var c struct{Binary,Endpoint,Auth string}
 data,e:=os.ReadFile(os.Getenv("ASB_ACK_CONFIG"));if e!=nil||json.Unmarshal(data,&c)!=nil{os.Exit(2)}
 cmd:=exec.Command(c.Binary,"--broker-url",c.Endpoint,"--token","synthetic-legacy","--auth-dir",c.Auth)
 cmd.Stdin,cmd.Stdout,cmd.Stderr=os.Stdin,os.Stdout,os.Stderr
 if cmd.Run()!=nil {os.Exit(2)}
}
`

func TestAckReleasedOldClientAutomaticDowngrade(t *testing.T) {
	binary := os.Getenv("ASB_ACK_OLD_CLIENT_BINARY")
	if binary == "" {
		t.Skip("opt-in: integrity-verified 0.17.2 binary and ASB_ACK_OLD_CLIENT_SHA256")
	}
	if runtime.GOOS == "windows" {
		t.Skip("synthetic named Codex host uses Unix process ancestry")
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal("old release binary unavailable")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != os.Getenv("ASB_ACK_OLD_CLIENT_SHA256") {
		t.Fatal("old release binary integrity mismatch")
	}
	version, err := exec.Command(binary, "--version").Output()
	if err != nil || strings.TrimSpace(string(version)) != "asb-client 0.17.2" {
		t.Fatal("test requires released 0.17.2")
	}
	dir := t.TempDir()
	helperSource := filepath.Join(dir, "host.go")
	helper := filepath.Join(dir, "codex")
	if os.WriteFile(helperSource, []byte(ackBinaryHost), 0600) != nil {
		t.Fatal("host fixture write")
	}
	build := exec.Command("go", "build", "-o", helper, helperSource)
	if build.Run() != nil {
		t.Fatal("synthetic host build failed")
	}
	s, clock := clockedService(t, "")
	// All clock reads after startup happen under the broker lock.
	s.mu.Lock()
	*clock = time.Now()
	s.mu.Unlock()
	var storageReplies, deliveries atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "agent-session-broker", Version: "0.17.3"}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{Experimental: map[string]any{protocol.MessageAckCapability: true, protocol.MessageSentAtCapability: true}},
	})
	mcp.AddTool(server, &mcp.Tool{Name: "register_session"}, func(ctx context.Context, _ *mcp.CallToolRequest, r RegisterRequest) (*mcp.CallToolResult, RegisterResult, error) {
		v, e := s.RegisterSession(ctx, r)
		if v.Code == CodeStorage {
			storageReplies.Add(1)
		}
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "resume_session"}, func(ctx context.Context, _ *mcp.CallToolRequest, r ResumeRequest) (*mcp.CallToolResult, RegisterResult, error) {
		v, e := s.ResumeSession(ctx, r)
		if v.Code == CodeStorage {
			storageReplies.Add(1)
		}
		return nil, v, e
	})
	server.AddTool(&mcp.Tool{Name: "renew_lease", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in RenewRequest
		if json.Unmarshal(r.Params.Arguments, &in) != nil {
			return nil, context.Canceled
		}
		v, e := s.RenewLease(ctx, in)
		return &mcp.CallToolResult{StructuredContent: v}, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "probe_response"}, func(ctx context.Context, _ *mcp.CallToolRequest, r ProbeAnswer) (*mcp.CallToolResult, ProbeResult, error) {
		v, e := s.AnswerProbe(ctx, r)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "wait_for_message"}, func(ctx context.Context, _ *mcp.CallToolRequest, r protocol.IngressRequest) (*mcp.CallToolResult, Message, error) {
		v, e := s.WaitForIngressAck(ctx, r)
		if e == nil && v.Body == "m1" {
			deliveries.Add(1)
		}
		return nil, v, e
	})
	type selector struct {
		Name string `json:"name"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "get_session"}, func(ctx context.Context, _ *mcp.CallToolRequest, r selector) (*mcp.CallToolResult, Session, error) {
		v, e := s.GetSession(ctx, r.Name)
		return nil, v, e
	})
	server.AddTool(&mcp.Tool{Name: "list_sessions", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		v, e := s.ListSessions(ctx)
		data, _ := json.Marshal(v)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, e
	})
	server.AddTool(&mcp.Tool{Name: "session_token_info", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{StructuredContent: map[string]any{"scopes": []string{"session:register", "session:read", "message:receive", "message:send"}}}, nil
	})
	remote := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/transport-policy" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"project":"test","allow_http":true}`)
			return
		}
		remote.ServeHTTP(w, r)
	}))
	defer httpServer.Close()
	endpoint := httpServer.URL + "/mcp?project=test"
	auth, home, cwd := t.TempDir(), t.TempDir(), t.TempDir()
	local, err := clientstate.Open(auth)
	if err != nil {
		t.Fatal("synthetic state open")
	}
	clientID, err := local.ClientID()
	if err != nil {
		t.Fatal("synthetic client identity")
	}
	req := durableRegister(1, "recipient", clientID, durableSecretA, "ack-seed", "ack-seed")
	req.Harness = "codex"
	req.Holder = nil
	req.CWD = cwd
	req.Capabilities = []string{protocol.MessageAckCapability, protocol.MessageSentAtCapability}
	registered, err := s.RegisterSession(t.Context(), req)
	if err != nil || registered.Code != "" {
		t.Fatal("ack seed registration")
	}
	expiryQueueSend(t, s, "m1")
	first, err := s.WaitForIngress(t.Context(), req.Name, req.ChannelID)
	if err != nil || first.DeliverySequence != 1 {
		t.Fatal("ack seed issuance")
	}
	key := clientstate.RegistrationKey(endpoint, req.Harness, req.SessionID)
	err = local.SaveRegistration(key, clientstate.Registration{RegistrationID: registered.Session.RegistrationID, Name: req.Name, Secret: req.Secret, Revision: registered.Session.Revision, Project: "test", Harness: req.Harness, SessionID: req.SessionID})
	if err != nil {
		t.Fatal("synthetic saved registration")
	}
	rollout := filepath.Join(home, ".codex", "sessions", "2026", "10", "25")
	if os.MkdirAll(rollout, 0700) != nil {
		t.Fatal("synthetic rollout directory")
	}
	meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": req.SessionID, "cwd": cwd}})
	if os.WriteFile(filepath.Join(rollout, "rollout-synthetic-"+req.SessionID+".jsonl"), append(meta, '\n'), 0600) != nil {
		t.Fatal("synthetic rollout metadata")
	}
	config, _ := json.Marshal(map[string]string{"Binary": binary, "Endpoint": endpoint, "Auth": auth})
	configPath, receipt := filepath.Join(dir, "config.json"), filepath.Join(dir, "receipt.json")
	if os.WriteFile(configPath, config, 0600) != nil {
		t.Fatal("synthetic host config")
	}
	cmd := exec.Command(helper)
	cmd.Dir = cwd
	cmd.Stderr = io.Discard
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + os.Getenv("TMPDIR"), "HOME=" + home, "USERPROFILE=" + home, "CODEX_HOME=" + filepath.Join(home, ".codex"), "CODEX_THREAD_ID=" + req.SessionID, "ASB_ACK_CONFIG=" + configPath, "ASB_ACK_RECEIPT=" + receipt}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	connection, err := mcp.NewClient(&mcp.Implementation{Name: "synthetic-harness", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: time.Second}, nil)
	if err != nil {
		t.Fatal("released client initialize failed")
	}
	defer connection.Close()
	wait := func(label string, predicate func() bool) {
		t.Helper()
		until := time.Now().Add(12 * time.Second)
		for time.Now().Before(until) {
			if predicate() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal(label)
	}
	wait("old binary did not retry storage without manual attachment", func() bool { return storageReplies.Load() > 0 })
	if deliveries.Load() != 0 {
		t.Fatal("old binary consumed protected payload early")
	}
	s.mu.Lock()
	e := s.byRegistration[registered.Session.RegistrationID]
	deadline := e.ackProtectedUntil
	if deadline.IsZero() {
		s.mu.Unlock()
		t.Fatal("missing fixed protection deadline")
	}
	*clock = deadline
	s.mu.Unlock()
	start := time.Now()
	wait("old binary did not automatically resume and receive after protection", func() bool {
		raw, err := os.ReadFile(receipt)
		if err != nil {
			return false
		}
		var got protocol.DeliveredMessage
		return json.Unmarshal(raw, &got) == nil && got.Body == "m1" && got.SentAt.Equal(first.SentAt)
	})
	if deliveries.Load() != 1 || time.Since(start) > 12*time.Second {
		t.Fatal("old binary delivery exceeded one backoff or duplicated")
	}
	t.Logf("released 0.17.2: storage retry -> automatic legacy resume -> same message/SentAt host receipt; fake hold 10m, retry %.3fs", time.Since(start).Seconds())
}
