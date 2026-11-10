package serverruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Explicit opt-in: real Haiku calls, isolated homes and a blank loopback runtime.
func TestMisuseK1RealClaudeNameReuse(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("local macOS real-host scene")
	}
	if os.Getenv("ASB_MISUSE_K1_CLAUDE") != "1" {
		t.Skip("real Claude opt-in")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()
	dir := t.TempDir()
	store, err := controlpanel.OpenSQLite(filepath.Join(dir, "control"))
	if err != nil {
		t.Fatal("control open")
	}
	if store.Setup("misuse-only-admin-password") != nil || store.CreateProject("test") != nil || store.SetInstanceHTTPPolicy("admin", true) != nil || store.SetScopeIssuanceReady("admin", true) != nil {
		t.Fatal("control setup")
	}
	ta, err := store.IssueProjectTokenWithOptionsFor("admin", "test", "K1-A", controlpanel.ProjectTokenOptions{Scopes: phase6AutoScopes(), UnlimitedAutoBindings: true})
	if err != nil {
		t.Fatal("A credential")
	}
	tb, err := store.IssueProjectTokenWithOptionsFor("admin", "test", "K1-B", controlpanel.ProjectTokenOptions{Scopes: phase6AutoScopes(), UnlimitedAutoBindings: true})
	if err != nil {
		t.Fatal("B credential")
	}
	store.Close()
	remote := broker.NewRemoteService()
	trace := &misuseSceneBackend{protectedBackend: newProtectedBackend("test", remote)}
	h, closeHandler, err := newManagedHandler(dir, func(groups.ProjectDirectory) (mcpserver.Backend, error) {
		return trace, nil
	})
	if err != nil {
		t.Fatal("handler")
	}

	server := httptest.NewServer(h)
	defer func() { cancel(); _ = closeHandler(); server.CloseClientConnections(); server.Close() }()
	binary := filepath.Join(t.TempDir(), "asb-client")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/asb-client")
	if build.Run() != nil {
		t.Fatal("client build")
	}
	a := misuseClaudeHost(t, ctx, binary, server.URL+"/mcp?project=test", ta, "A")
	b := misuseClaudeHost(t, ctx, binary, server.URL+"/mcp?project=test", tb, "B")
	a.ask("Call x-broker attach_session once, then auth_status. If registered true, respond with the concatenation of K1_READY_ and A. Do not send messages.", "K1_READY_A")
	deadline := time.Now().Add(5 * time.Second)
	for {
		ready, err := remote.GetSession(ctx, "K1-A")
		if err == nil && ready.AutomaticDeliveryReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("real A ingress not ready: registered=%t code=%s", err == nil, ready.ErrorCode)
		}
		time.Sleep(50 * time.Millisecond)
	}
	b.ask("Call x-broker attach_session once, then auth_status. Call get_session for K1-A, use its registrationId as expectRegistrationId in send_message from K1-B to K1-A with body K1_FIRST_DELIVERY. Respond with the concatenation of K1_SENT_ and B only after accepted true.", "K1_SENT_B")
	a.ask("Read the incoming K1_FIRST_DELIVERY from K1-B. Do not reply to it. Respond with concatenation of K1_RECEIVED_ and A only when you have actually received it.", "K1_RECEIVED_A")
	trace.mu.Lock()
	receivedFirst := trace.receivedFirst
	sentFirst := trace.sentFirst
	trace.mu.Unlock()
	if !receivedFirst || !sentFirst {
		t.Fatal("real B-to-A delivery not observed by runtime")
	}
	old, err := remote.GetSession(ctx, "K1-B")
	if err != nil {
		t.Fatal("B registration missing")
	}
	// The real A manager retires B, exercising credential cleanup as well as routing.
	a.ask("Use retire_session registrationId="+old.RegistrationID+" to retire K1-B. Do not call get_session. Respond with concatenation of K1_RETIRED_ and B after success.", "K1_RETIRED_B")
	b.stop()
	cReq := protectedRequest(903, "K1-B")
	c, err := remote.RegisterSession(ctx, cReq)
	if err != nil || c.Code != "" {
		t.Fatal("replacement registration")
	}
	trace.mu.Lock()
	getsBefore := trace.gets
	trace.mu.Unlock()
	a.ask("Test stale-name protection: call send_message with from K1-A, to K1-B, body K1_MUST_NOT_DELIVER. OMIT expectRegistrationId. Do not call get_session or list_sessions first. The expected result is recipient-changed. Respond with concatenation of K1_FENCED_ and A only if that exact refusal occurs.", "K1_FENCED_A")
	trace.mu.Lock()
	fenced := trace.fenced
	getsAfter := trace.gets
	trace.mu.Unlock()
	if !fenced || getsAfter != getsBefore {
		t.Fatal("real name-only refusal without prior get not observed")
	}
	current, err := remote.GetSession(ctx, c.Session.RegistrationID)
	if err != nil || current.PendingMessages != 0 {
		t.Fatal("stale reply queued to replacement")
	}
	a.ask("Now call get_session for K1-B, then send_message from K1-A to K1-B with body K1_AFTER_CONFIRM and OMIT expectRegistrationId. Respond with concatenation of K1_CONFIRMED_ and A only after accepted true.", "K1_CONFIRMED_A")
	trace.mu.Lock()
	confirmed := trace.confirmed
	getsAfter = trace.gets
	trace.mu.Unlock()
	if !confirmed || getsAfter <= getsBefore {
		t.Fatal("get_session then name-only accepted reply not observed")
	}
	deliveryCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	received, err := remote.WaitForIngress(deliveryCtx, "K1-B", cReq.ChannelID)
	if err != nil || received.Body != "K1_AFTER_CONFIRM" {
		t.Fatal("confirmed message not received")
	}
	t.Log("two real Haiku hosts: actual delivery, retirement, replacement queue=0, confirmation delivery PASS")
}

type misuseHost struct {
	t       *testing.T
	cmd     *exec.Cmd
	in      io.WriteCloser
	out     *bufio.Scanner
	stopped bool
}

func (h *misuseHost) stop() {
	if !h.stopped {
		h.stopped = true
		h.in.Close()
		misuseKillHost(h.cmd.Process.Pid)
		_ = h.cmd.Wait()
	}
}
func (h *misuseHost) ask(prompt, marker string) {
	h.t.Helper()
	_, err := io.WriteString(h.in, prompt+"\t"+marker+"\n")
	if err != nil || !h.out.Scan() || h.out.Text() != "DONE" {
		h.t.Fatal("isolated Claude step failed: " + marker + " (" + h.out.Text() + ")")
	}
	h.t.Log(marker)
}
func misuseClaudeHost(t *testing.T, ctx context.Context, binary, endpoint, token, label string) *misuseHost {
	t.Helper()
	root, err := os.MkdirTemp(os.TempDir(), "asb-k1-"+label+"-")
	if err != nil {
		t.Fatal("isolated host root")
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	// Existing OAuth is read into RAM only; no credential is copied to a file or log.
	originalHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal("home lookup")
	}
	raw, err := os.ReadFile(filepath.Join(originalHome, ".claude", ".credentials.json"))
	if err != nil {
		t.Fatal("existing Claude OAuth unavailable")
	}
	var auth struct {
		OAuth struct {
			Token string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(raw, &auth) != nil || auth.OAuth.Token == "" {
		t.Fatal("existing Claude OAuth unavailable")
	}
	python := os.Getenv("ASB_MISUSE_K1_PYTHON")
	if python == "" {
		python = "python3"
	}
	driver, err := filepath.Abs("testdata/misuse_k1_claude.py")
	if err != nil {
		t.Fatal("driver path")
	}
	cmd := exec.CommandContext(ctx, python, driver)
	for _, key := range []string{"PATH", "LANG", "SDKROOT", "SHELL"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, "ANTHROPIC_AUTH_TOKEN="+auth.OAuth.Token)
	cmd.Stderr = io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("driver stdin")
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("driver stdout")
	}
	if cmd.Start() != nil {
		t.Fatal("driver start")
	}
	h := &misuseHost{t: t, cmd: cmd, in: in, out: bufio.NewScanner(out)}
	t.Cleanup(h.stop)
	config := map[string]string{"root": root, "binary": binary, "endpoint": endpoint, "token": token, "name": "K1-" + label}
	if json.NewEncoder(in).Encode(config) != nil {
		t.Fatal("driver config")
	}
	if !h.out.Scan() || h.out.Text() != "READY" {
		t.Fatal("isolated Claude startup: " + h.out.Text())
	}
	t.Logf("isolated Haiku host %s ready pid=%d", label, cmd.Process.Pid)
	return h
}

func misuseKillHost(pid int) {
	raw, _ := exec.Command("pgrep", "-P", strconv.Itoa(pid)).Output()
	for _, field := range strings.Fields(string(raw)) {
		child, err := strconv.Atoi(field)
		if err == nil {
			misuseKillHost(child)
		}
	}
	process, err := os.FindProcess(pid)
	if err == nil {
		_ = process.Kill()
	}
}

// Assertions use server-owned observations, rather than trusting a model's reply.
type misuseSceneBackend struct {
	*protectedBackend
	mu                                          sync.Mutex
	sentFirst, receivedFirst, fenced, confirmed bool
	gets                                        int
}

func (b *misuseSceneBackend) SendMessage(ctx context.Context, m broker.Message) (broker.SendResult, error) {
	out, err := b.protectedBackend.SendMessage(ctx, m)
	b.mu.Lock()
	defer b.mu.Unlock()
	switch m.Body {
	case "K1_FIRST_DELIVERY":
		b.sentFirst = err == nil && out.Accepted && m.From == "K1-B" && m.To == "K1-A"
	case "K1_MUST_NOT_DELIVER":
		b.fenced = err == nil && !out.Accepted && out.Code == "recipient-changed" && m.ExpectRegistrationID == nil
	case "K1_AFTER_CONFIRM":
		b.confirmed = err == nil && out.Accepted && m.ExpectRegistrationID == nil
	}
	return out, err
}
func (b *misuseSceneBackend) GetSession(ctx context.Context, selector string) (broker.Session, error) {
	out, err := b.protectedBackend.GetSession(ctx, selector)
	b.mu.Lock()
	b.gets++
	b.mu.Unlock()
	return out, err
}
func (b *misuseSceneBackend) WaitForIngressAck(ctx context.Context, r protocol.IngressRequest) (broker.Message, error) {
	out, err := b.protectedBackend.WaitForIngressAck(ctx, r)
	b.mu.Lock()
	if err == nil && out.Body == "K1_FIRST_DELIVERY" && out.From == "K1-B" && r.Recipient == "K1-A" {
		b.receivedFirst = true
	}
	b.mu.Unlock()
	return out, err
}
func (b *misuseSceneBackend) WaitForIngress(ctx context.Context, name, lease string) (broker.Message, error) {
	out, err := b.protectedBackend.WaitForIngress(ctx, name, lease)
	b.mu.Lock()
	if err == nil && out.Body == "K1_FIRST_DELIVERY" && out.From == "K1-B" && name == "K1-A" {
		b.receivedFirst = true
	}
	b.mu.Unlock()
	return out, err
}
