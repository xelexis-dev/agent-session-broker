package codex

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

func TestExternalProcessProbe(t *testing.T) {
	if os.Getenv("BROKER_EXTERNAL_PROBE_HELPER") == "1" {
		io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("external attachment supports Windows and Linux")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	name := "codex"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, bytes, 0700); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(path, "-test.run=^TestExternalProcessProbe$")
	child.Env = append(os.Environ(), "BROKER_EXTERNAL_PROBE_HELPER=1")
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { input.Close(); child.Process.Kill(); child.Wait() }()
	key, err := externalProcessKey(context.Background(), child.Process.Pid)
	if err != nil || key == "" {
		t.Fatalf("live key=%q %v", key, err)
	}
	// Exercise the consumer of the real OS probe key, not just its existence.
	// A test helper stands in for Codex; no user session receives a message.
	a, request, _ := externalPushFixture(t)
	request.PID = child.Process.Pid
	a.externalProbe = nil
	queued := false
	a.externalQueue = func(_ context.Context, executable, cwd string, args []string) error {
		samePath := executable == path
		if runtime.GOOS == "windows" {
			samePath = strings.EqualFold(executable, path)
		}
		if !samePath || cwd != request.CWD || len(args) != 5 || args[0] != "queue" || args[1] != "--thread" || args[2] != request.SessionID || args[3] != "--message" {
			t.Fatal("native process key did not select its pinned queue command")
		}
		queued = true
		return nil
	}
	if err := a.PushExternal(context.Background(), request, key, broker.Message{From: "sender", To: request.Name, Body: "native probe delivery"}); err != nil {
		t.Fatalf("native process was registered but could not receive: %v", err)
	}
	if !queued {
		t.Fatal("native process queue not invoked")
	}
	if _, err := externalProcessKey(context.Background(), os.Getpid()); err == nil {
		t.Fatal("accepted non-Codex executable")
	}
	input.Close()
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := externalProcessKey(context.Background(), child.Process.Pid); err == nil {
		t.Fatal("accepted exited process")
	}
}
