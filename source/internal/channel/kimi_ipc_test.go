//go:build darwin || linux

package channel

import (
	"context"
	"encoding/json"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKimiHookProcess(t *testing.T) {
	if os.Getenv("ASB_KIMI_TEST_HELPER") != "1" {
		return
	}
	err := RunKimiHook(context.Background(), []string{"--state-dir", os.Getenv("ASB_KIMI_TEST_STATE")}, os.Stdin, os.Stdout)
	if err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}
func TestKimiHookIPCChecksPeerAndDelivers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	state := shortSocketDir(t, "asb-kimi-ipc-")
	entry := filepath.Join(state, "@moonshot-ai/kimi-code/dist/main.mjs")
	if e := os.MkdirAll(filepath.Dir(entry), 0700); e != nil {
		t.Fatal(e)
	}
	js := `import {spawn} from 'node:child_process'; const child=spawn(process.argv[2],['-test.run=^TestKimiHookProcess$'],{env:process.env,stdio:['pipe','pipe','ignore']}); process.stdin.pipe(child.stdin); child.stdout.pipe(process.stdout); setInterval(()=>{},1000);`
	if e := os.WriteFile(entry, []byte(js), 0600); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(node, entry, os.Args[0])
	cmd.Dir = state
	cmd.Env = append(os.Environ(), "ASB_KIMI_TEST_HELPER=1", "ASB_KIMI_TEST_STATE="+state)
	input, _ := cmd.StdinPipe()
	output, _ := cmd.StdoutPipe()
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	k := newKimiClient(cmd.Process.Pid, state)
	stop := k.listen(ctx, state)
	defer stop()
	if k.problem != nil {
		t.Fatal(k.problem)
	}
	socket, e := kimiSocketPath(state, cmd.Process.Pid)
	if e != nil {
		t.Fatal(e)
	}
	info, e := os.Stat(socket)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("socket mode", e)
	}
	// The test process has no Kimi ancestor. Its claimed session cannot bind us.
	foreign, e := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
	if e != nil {
		t.Fatal(e)
	}
	_ = foreign.SetDeadline(time.Now().Add(time.Second))
	raw := `{"event":"SessionStart","session_id":"` + kimiTestSession + `","cwd":` + strconvJSON(state) + `}`
	_, _ = foreign.Write([]byte(raw))
	_ = foreign.CloseWrite()
	data, _ := io.ReadAll(foreign)
	_ = foreign.Close()
	if len(data) != 0 || k.session != "" {
		t.Fatal("foreign process supplied identity")
	}
	if e := k.queue.deliver(t.Context(), broker.Message{Body: "actual-ipc-message"}); e != nil {
		t.Fatal(e)
	}
	payload := `{"hook_event_name":"UserPromptSubmit","session_id":"` + kimiTestSession + `","cwd":` + strconvJSON(state) + `}`
	_, _ = io.WriteString(input, payload)
	_ = input.Close()
	result := make(chan map[string]any, 1)
	go func() { var out map[string]any; _ = json.NewDecoder(output).Decode(&out); result <- out }()
	select {
	case out := <-result:
		message, _ := out["message"].(string)
		if !strings.Contains(message, "actual-ipc-message") {
			t.Fatal(out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("valid peer hook did not receive batch")
	}
	id, e := k.identify(t.Context(), nil)
	if e != nil || id.SessionID != "11111111-1111-4111-8111-111111111111" {
		t.Fatal(id, e)
	}
}
func strconvJSON(s string) string { b, _ := json.Marshal(s); return string(b) }
func TestKimiSocketLongStateUsesPrivateShortDirectory(t *testing.T) {
	state := filepath.Join(t.TempDir(), strings.Repeat("deep", 40))
	if e := os.MkdirAll(state, 0700); e != nil {
		t.Fatal(e)
	}
	path, e := kimiSocketPath(state, os.Getpid())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Dir(path)) })
	if len(path) >= 104 {
		t.Fatal("socket too long")
	}
	info, e := os.Lstat(filepath.Dir(path))
	if e != nil || info.Mode().Perm() != 0700 {
		t.Fatal("fallback not private", e)
	}
	if e := os.Chmod(filepath.Dir(path), 0755); e != nil {
		t.Fatal(e)
	}
	if _, e := kimiSocketPath(state, os.Getpid()); e == nil {
		t.Fatal("unsafe existing fallback accepted")
	}
}
func TestKimiHookOversizeFailsOpen(t *testing.T) {
	start := time.Now()
	if e := RunKimiHook(t.Context(), nil, strings.NewReader(strings.Repeat("x", (64<<10)+1)), io.Discard); e != nil {
		t.Fatal(e)
	}
	if time.Since(start) > time.Second {
		t.Fatal("oversize hook blocked")
	}
}
