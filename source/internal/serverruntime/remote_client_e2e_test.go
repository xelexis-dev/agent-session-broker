package serverruntime

// Opt-in remote client E2E for the Windows x64 1.0 check
// (agent/docs/windows-10-verify.md). A real asb-client runs under the
// synthetic no-model Codex relay (phase6Relay) against an already running
// broker. Inputs: ASB_REMOTE_E2E_CLIENT (client binary), ASB_REMOTE_E2E_URL
// (broker origin, project "test") and ASB_REMOTE_E2E_TOKEN_FILE (project token
// with automatic registration). Logs carry fixed labels and booleans only.
import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type remoteHarness struct {
	session         *mcp.ClientSession
	cmd             *exec.Cmd
	auth, queue, id string
}

func remoteStart(t *testing.T, relay, endpoint, token, nickname, auth, id string) *remoteHarness {
	t.Helper()
	home, cwd := t.TempDir(), t.TempDir()
	sessions := filepath.Join(home, "sessions", "2026", "11", "10")
	meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": id, "cwd": cwd}})
	if os.MkdirAll(sessions, 0700) != nil || os.WriteFile(filepath.Join(sessions, "rollout-e2e-"+id+".jsonl"), append(meta, '\n'), 0600) != nil {
		t.Fatal("synthetic rollout write failed")
	}
	h := &remoteHarness{auth: auth, queue: filepath.Join(home, "queue.json"), id: id}
	h.cmd = exec.Command(relay, os.Getenv("ASB_REMOTE_E2E_CLIENT"), "--broker-url", endpoint, "--token", token, "--auth-dir", auth, "--nickname", nickname)
	h.cmd.Dir = cwd
	h.cmd.Env = append(os.Environ(), "CODEX_HOME="+home, "CODEX_THREAD_ID="+id, "PHASE6_QUEUE="+h.queue)
	h.cmd.Stderr = io.Discard
	session, err := mcp.NewClient(&mcp.Implementation{Name: "remote-e2e-harness", Version: "test"}, nil).Connect(t.Context(), &mcp.CommandTransport{Command: h.cmd, TerminateDuration: 3 * time.Second}, nil)
	if err != nil {
		t.Fatal("stdio client connect failed")
	}
	h.session = session
	t.Cleanup(h.close)
	return h
}

func (h *remoteHarness) close() {
	if h.session != nil {
		_ = h.session.Close()
		h.session = nil
	}
}

// call carries the thread identity in request metadata, as Codex does.
func (h *remoteHarness) call(t *testing.T, name string, args any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	r, err := h.session.CallTool(ctx, &mcp.CallToolParams{Meta: mcp.Meta{"threadId": h.id}, Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("stdio tool %s transport failed", name)
	}
	return r
}

// attach is the user-requested first registration; token projects assign the name.
func (h *remoteHarness) attach(t *testing.T) map[string]any {
	t.Helper()
	if r := h.call(t, "attach_session", map[string]any{}); r.IsError {
		t.Fatalf("attach_session refused: %.300q", remoteErrorText(r))
	}
	return h.registered(t)
}

// registered waits without attaching, so a saved registration must resume by itself.
func (h *remoteHarness) registered(t *testing.T) map[string]any {
	t.Helper()
	var s map[string]any
	for start := time.Now(); time.Since(start) < 30*time.Second; time.Sleep(250 * time.Millisecond) {
		if s = phase6Decode[map[string]any](t, h.call(t, "auth_status", struct{}{})); s["registered"] == true {
			return s
		}
	}
	// Fixed diagnostic fields only; never credentials or codes.
	t.Fatalf("timed out waiting for registration: status=%v registrationStatus=%v errorCode=%v message=%.200v", s["status"], s["registrationStatus"], s["errorCode"], s["message"])
	return nil
}

func (h *remoteHarness) send(t *testing.T, from, to, body string) (broker.SendResult, bool) {
	t.Helper()
	r := h.call(t, "send_message", map[string]string{"from": from, "to": to, "body": body})
	if r.IsError {
		return broker.SendResult{Message: remoteErrorText(r)}, false
	}
	return phase6Decode[broker.SendResult](t, r), true
}

// remoteErrorText returns a tool error's product message; the client never puts credentials in it.
func remoteErrorText(r *mcp.CallToolResult) string {
	if len(r.Content) == 1 {
		if text, ok := r.Content[0].(*mcp.TextContent); ok {
			return text.Text
		}
	}
	return ""
}

func (h *remoteHarness) peer(t *testing.T, name string) (broker.Session, bool) {
	t.Helper()
	r := h.call(t, "get_session", map[string]string{"name": name})
	if r.IsError {
		return broker.Session{}, false
	}
	return phase6Decode[broker.Session](t, r), true
}

func (h *remoteHarness) queued(marker string) bool {
	data, _ := os.ReadFile(h.queue)
	return bytes.Contains(data, []byte(marker))
}

func remoteWait(t *testing.T, label string, limit time.Duration, f func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for time.Since(start) < limit {
		if f() {
			return time.Since(start)
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", label)
	return 0
}

func remoteUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func TestRemoteClientE2E(t *testing.T) {
	binary, origin, tokenFile := os.Getenv("ASB_REMOTE_E2E_CLIENT"), os.Getenv("ASB_REMOTE_E2E_URL"), os.Getenv("ASB_REMOTE_E2E_TOKEN_FILE")
	if binary == "" || origin == "" || tokenFile == "" {
		t.Skip("opt-in remote client E2E: set ASB_REMOTE_E2E_CLIENT, ASB_REMOTE_E2E_URL and ASB_REMOTE_E2E_TOKEN_FILE")
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal("token file unreadable")
	}
	token := strings.TrimSpace(string(raw))
	endpoint := strings.TrimRight(origin, "/") + "/mcp?project=test"
	relay := phase6BuildRelay(t)
	run := remoteUUID()[:8]
	aID, bID, aAuth, bAuth := remoteUUID(), remoteUUID(), t.TempDir(), t.TempDir()

	// 1. Register two sessions through direct registration.
	a := remoteStart(t, relay, endpoint, token, "e2e-a-"+run, aAuth, aID)
	b := remoteStart(t, relay, endpoint, token, "e2e-b-"+run, bAuth, bID)
	as, bs := a.attach(t), b.attach(t)
	aName, _ := as["name"].(string)
	bName, _ := bs["name"].(string)
	aReg, _ := as["registrationId"].(string)
	bReg, _ := bs["registrationId"].(string)
	if aName == "" || bName == "" || aReg == "" || bReg == "" || aReg == bReg {
		t.Fatal("registration returned no distinct name/registration")
	}
	remoteWait(t, "delivery readiness", 30*time.Second, func() bool {
		pa, okA := b.peer(t, aName)
		pb, okB := a.peer(t, bName)
		return okA && okB && pa.AutomaticDeliveryReady && pb.AutomaticDeliveryReady
	})
	t.Logf("PASS register: two sessions, admissionMode a=%v b=%v, both automaticDeliveryReady", as["admissionMode"], bs["admissionMode"])

	// 2. Two-way delivery to idle receivers through the native Codex queue.
	m1, m2 := "e2e-a2b-"+run, "e2e-b2a-"+run
	if r, ok := a.send(t, aName, bName, m1); !ok || !r.Accepted {
		t.Fatal("a->b send not accepted")
	}
	d1 := remoteWait(t, "a->b idle receive", 30*time.Second, func() bool { return b.queued(m1) })
	if r, ok := b.send(t, bName, aName, m2); !ok || !r.Accepted {
		t.Fatal("b->a send not accepted")
	}
	d2 := remoteWait(t, "b->a idle receive", 30*time.Second, func() bool { return a.queued(m2) })
	t.Logf("PASS two-way idle receive: a->b %dms, b->a %dms (native queue)", d1.Milliseconds(), d2.Milliseconds())

	// 3. Receive while a turn is active: held for the next hook, not queued.
	hook := func(event string) string {
		r := a.call(t, "ingress_hook", map[string]string{"session_id": aID, "hook_event_name": event, "turn_id": "turn-" + run})
		if r.IsError {
			t.Fatalf("ingress_hook %s refused: %.300q", event, remoteErrorText(r))
		}
		data, _ := json.Marshal(r)
		return string(data)
	}
	_ = os.Remove(a.queue)
	hook("UserPromptSubmit")
	m3 := "e2e-turn-" + run
	if r, ok := b.send(t, bName, aName, m3); !ok || !r.Accepted {
		t.Fatal("in-turn send not accepted")
	}
	d3 := remoteWait(t, "in-turn hook receive", 30*time.Second, func() bool {
		return strings.Contains(hook("PreToolUse"), m3) || strings.Contains(hook("PostToolUse"), m3)
	})
	hook("Stop")
	if a.queued(m3) {
		t.Fatal("in-turn message also went to the idle queue")
	}
	t.Logf("PASS in-turn receive: delivered in a tool hook after %dms, idle queue untouched", d3.Milliseconds())

	// 4. Reconnect with the saved state: same registration ID and name.
	a.close()
	a = remoteStart(t, relay, endpoint, token, "ignored-on-resume", aAuth, aID)
	again := a.registered(t)
	if again["registrationId"] != aReg || again["name"] != aName {
		t.Fatal("reconnect changed the registration")
	}
	m4 := "e2e-after-reconnect-" + run
	_ = os.Remove(a.queue)
	remoteWait(t, "readiness after reconnect", 30*time.Second, func() bool { p, ok := b.peer(t, aName); return ok && p.AutomaticDeliveryReady })
	if r, ok := b.send(t, bName, aName, m4); !ok || !r.Accepted {
		t.Fatal("send after reconnect not accepted")
	}
	remoteWait(t, "receive after reconnect", 30*time.Second, func() bool { return a.queued(m4) })
	t.Log("PASS reconnect: same registrationId and name, receive resumes")

	// 5. Kill b's harness process: the server must notice, and a send to the
	// registration that is not alive must fail instead of being accepted.
	if b.cmd.Process == nil || b.cmd.Process.Kill() != nil {
		t.Fatal("harness kill failed")
	}
	// The relay passes this test's pipes straight to the client; closing them
	// is what the OS does to a killed harness's stdio.
	b.close()
	seen := broker.Session{Status: "absent"}
	d5 := remoteWait(t, "server notices killed harness", 90*time.Second, func() bool {
		p, ok := a.peer(t, bName)
		if ok {
			seen = p
		}
		return !ok || (!p.AutomaticDeliveryReady && p.Status != broker.StatusAlive && p.Status != broker.StatusIdle && p.Status != broker.StatusWorking)
	})
	r5, ok5 := a.send(t, aName, bName, "e2e-to-dead-"+run)
	if ok5 && r5.Accepted {
		t.Fatal("send to a registration that is not alive was accepted")
	}
	t.Logf("PASS not-alive: server state %q after %dms, send refused (toolError=%v code=%q message=%.160q)", seen.Status, d5.Milliseconds(), !ok5, r5.Code, r5.Message)

	// 6. Disconnect: stop_session detaches a centrally; the local stdio stays.
	b = remoteStart(t, relay, endpoint, token, "ignored-on-resume", bAuth, bID)
	if s := b.registered(t); s["registrationId"] != bReg {
		t.Fatal("b reconnect after kill changed the registration")
	}
	stopped := phase6Decode[map[string]any](t, a.call(t, "stop_session", map[string]string{"name": aName}))
	if stopped["ok"] != true {
		t.Fatal("stop_session did not detach")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := a.session.ListTools(ctx, nil); err != nil {
		t.Fatal("stop_session terminated the local stdio harness")
	}
	var detached broker.Session
	d6 := remoteWait(t, "server shows detached session", 30*time.Second, func() bool {
		p, ok := b.peer(t, aName)
		detached = p
		return !ok || (!p.AutomaticDeliveryReady && p.Status != broker.StatusAlive && p.Status != broker.StatusIdle && p.Status != broker.StatusWorking)
	})
	r6, ok6 := b.send(t, bName, aName, "e2e-to-detached-"+run)
	if ok6 && r6.Accepted {
		t.Fatal("send to a detached registration was accepted")
	}
	t.Logf("PASS disconnect: stop_session ok, peer sees %q after %dms, send refused (message=%.160q), local stdio alive", detached.Status, d6.Milliseconds(), r6.Message)
}
