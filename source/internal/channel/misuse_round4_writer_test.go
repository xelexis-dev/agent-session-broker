//go:build darwin || linux

package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
)

// Opt in with two actual CLI binaries; normal unit runs do not need a tag checkout.
func TestRound4ReleasedClientConcurrentWriter(t *testing.T) {
	if os.Getenv("ASB_ROUND4_OLD_CLIENT") == "" || os.Getenv("ASB_ROUND4_NEW_CLIENT") == "" {
		t.Skip("requires tag0.17.2 and feature-head asb-client binaries")
	}
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("test executable")
	}
	src, err := os.Open(executable)
	if err != nil {
		t.Fatal("copy test host")
	}
	defer src.Close()
	host := filepath.Join(root, "claude")
	dst, err := os.OpenFile(host, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal("create fake host")
	}
	_, err = io.Copy(dst, src)
	closeErr := dst.Close()
	if err != nil || closeErr != nil {
		t.Fatal("copy fake host")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, host, "-test.run=^TestRound4ClientHostHelper$", "-test.v")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "CLAUDE_PROJECT_DIR=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "ASB_ROUND4_HOST_ROOT="+root, "GORACE=atexit_sleep_ms=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal("isolated real-client writer probe failed (raw child output withheld)")
	}
	if !bytes.Contains(output, []byte("real CLI writers and prune verified")) {
		t.Fatal("probe completion absent")
	}
	t.Log("tag0.17.2 and feature CLI: simultaneous v1/v2 writers, old prune skips v2, v1 immutable after old exit")
}

func TestRound4ClientHostHelper(t *testing.T) {
	root := os.Getenv("ASB_ROUND4_HOST_ROOT")
	if root == "" {
		return
	}
	for _, name := range []string{"HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		dir := filepath.Join(root, name)
		if os.MkdirAll(dir, 0700) != nil {
			t.Fatal("isolated home")
		}
		t.Setenv(name, dir)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	if os.Unsetenv("CLAUDE_PROJECT_DIR") != nil {
		t.Fatal("unset nested-host project override")
	}
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer server.Close()
	endpoint := server.URL + "/mcp?project=round4"
	dir := filepath.Join(root, "state")
	state, err := clientstate.Open(dir)
	if err != nil {
		t.Fatal("state fixture")
	}
	client, err := state.ClientID()
	if err != nil {
		t.Fatal("client fixture")
	}
	id := "11111111-1111-4111-8111-111111119404"
	key := clientstate.SessionKey(brokerScope(endpoint, "round4"), "claude", id, client)
	release, err := state.LockSession(key)
	if err != nil {
		t.Fatal("hold competing client lock")
	}
	defer release()
	roster := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "sessions")
	os.MkdirAll(roster, 0700)
	raw, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "kind": "interactive", "cwd": root, "sessionId": id})
	if os.WriteFile(filepath.Join(roster, fmt.Sprintf("%d.json", os.Getpid())), raw, 0600) != nil {
		t.Fatal("fake parent roster")
	}
	oldPath := filepath.Join(dir, "diagnostics", key+".json")
	newPath := filepath.Join(dir, "diagnostics", "v2", key+".json")
	staleKey := clientstate.RegistrationKey("stale", "claude", id)
	staleV1 := filepath.Join(dir, "diagnostics", staleKey+".json")
	staleV2 := filepath.Join(dir, "diagnostics", "v2", staleKey+".json")
	os.MkdirAll(filepath.Dir(staleV2), 0700)
	fixture := []byte(`{"format":2,"summary":{},"events":[]}`)
	for _, p := range []string{staleV1, staleV2} {
		if os.WriteFile(p, fixture, 0600) != nil {
			t.Fatal("stale fixture")
		}
		old := time.Now().Add(-15 * 24 * time.Hour)
		os.Chtimes(p, old, old)
	}
	start := func(binary string) (*mcp.ClientSession, func()) {
		ctx, cancel := context.WithCancel(t.Context())
		cmd := exec.CommandContext(ctx, binary, "--broker-url", endpoint, "--token", strings.Join([]string{"asb", "p", "synthetic-round4"}, "_"), "--state-dir", dir)
		cmd.Stderr = io.Discard
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal("client stdin")
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal("client stdout")
		}
		if cmd.Start() != nil {
			t.Fatal("start actual client")
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		stop := sync.OnceFunc(func() {
			stdin.Close()
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("client exit timeout")
			}
		})
		t.Cleanup(stop)
		bound, end := context.WithTimeout(ctx, 5*time.Second)
		defer end()
		session, err := mcp.NewClient(&mcp.Implementation{Name: "round4-fake-host", Version: "1"}, nil).Connect(bound, &mcp.IOTransport{Reader: stdout, Writer: stdin}, nil)
		if err != nil {
			stop()
			t.Fatal("actual client initialize")
		}
		return session, func() { session.Close(); stop() }
	}
	status := func(session *mcp.ClientSession) error {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "auth_status", Arguments: map[string]any{}})
		if err != nil {
			return fmt.Errorf("auth_status failed")
		}
		safe, _ := json.Marshal(result)
		if !bytes.Contains(safe, []byte("owned-local")) {
			return fmt.Errorf("expected local lock refusal")
		}
		return nil
	}
	oldClient, stopOld := start(os.Getenv("ASB_ROUND4_OLD_CLIENT"))
	if status(oldClient) != nil {
		t.Fatal("old client did not reach real diagnostic writer")
	}
	if _, err = os.Stat(staleV1); !os.IsNotExist(err) {
		t.Fatal("old CLI startup prune did not run")
	}
	if _, err = os.Stat(staleV2); err != nil {
		t.Fatal("old prune entered v2 folder")
	}
	newClient, stopNew := start(os.Getenv("ASB_ROUND4_NEW_CLIENT"))
	defer stopNew()
	if status(newClient) != nil {
		t.Fatal("new client did not reach real diagnostic writer")
	}
	errors := make(chan error, 3)
	var wg sync.WaitGroup
	for _, session := range []*mcp.ClientSession{oldClient, newClient} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				if err := status(session); err != nil {
					errors <- err
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		os.Chtimes(filepath.Join(dir, "prune.stamp"), time.Now().Add(-25*time.Hour), time.Now().Add(-25*time.Hour))
		_, err := state.Prune(time.Now())
		errors <- err
	}()
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal("concurrent operation failed")
		}
	}
	stopOld()
	before, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal("v1 writer evidence absent")
	}
	for i := 0; i < 10; i++ {
		if status(newClient) != nil {
			t.Fatal("new writer after old exit")
		}
	}
	after, err := os.ReadFile(oldPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("new client changed v1 bytes")
	}
	for _, p := range []string{oldPath, newPath} {
		raw, err := os.ReadFile(p)
		var d struct {
			Events []json.RawMessage `json:"events"`
		}
		if err != nil || json.Unmarshal(raw, &d) != nil || len(d.Events) == 0 {
			t.Fatal("writer file corrupt or empty")
		}
	}
	loaded, err := state.LoadDiagnostics(key)
	if err != nil || len(loaded.Events) < 2 {
		t.Fatal("merged state unreadable")
	}
	t.Log("real CLI writers and prune verified")
}
