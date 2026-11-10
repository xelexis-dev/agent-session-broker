package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestDiscoveryNeverWaitsForClaudeIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	identify := func(ctx context.Context, _ *mcp.CallToolRequest) (broker.AttachRequest, error) {
		<-ctx.Done()
		return broker.AttachRequest{}, ctx.Err()
	}
	server := newIngressServer(ctx, &ingressUpstream{}, snapshotTools(), identify, nil)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	bound, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	began := time.Now()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "duplicate-startup", Version: "1"}, nil).Connect(bound, ct, nil)
	if err != nil {
		t.Fatal("initialize waited for parent identity")
	}
	defer client.Close()
	tools, err := client.ListTools(bound, nil)
	if err != nil || !hasTool(tools.Tools, "list_sessions") {
		t.Fatal("tools/list waited for unresolved parent identity instead of using snapshot")
	}
	t.Logf("initialize+tools/list=%s", time.Since(began))
}

// The fake tree has two live Claude processes with one conversation, but only
// 61530 is this client's ancestor. No real user processes or session files run.
func duplicateParent(t *testing.T) clientParentSource {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{61530, 62251} {
		data, _ := json.Marshal(map[string]any{"pid": pid, "kind": "interactive", "cwd": "/fixture", "sessionId": "11111111-1111-4111-8111-111111111111"})
		if err := os.WriteFile(filepath.Join(root, "sessions", fmt.Sprintf("%d.json", pid)), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	key := func(_ context.Context, pid int) (string, error) { return fmt.Sprintf("process-%d", pid), nil }
	return clientParentSource{
		ancestors: func() ([]int, error) { return []int{123, 61530, 1}, nil },
		classify: func(pid int) (string, error) {
			if pid == 61530 {
				return "claude", nil
			}
			return "", nil
		},
		processKey: key,
		inspectClaude: func(ctx context.Context, pid int) (broker.AttachRequest, error) {
			if pid != 61530 {
				return broker.AttachRequest{}, fmt.Errorf("selected non-parent")
			}
			return claudeParentSessionFile(ctx, pid, readClaudeSessionFile, key)
		},
	}
}

func startupClient(t *testing.T, args []string, parent clientParentSource) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	st, ct := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- runClientTransport(ctx, st, args, parent) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("client did not stop")
		}
	})
	bound, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	start := time.Now()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "fake-parent", Version: "1"}, nil).Connect(bound, ct, nil)
	if err != nil {
		t.Fatal("initialize exceeded 2s:", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	listed, err := cs.ListTools(bound, nil)
	if err != nil || !hasTool(listed.Tools, "auth_status") {
		t.Fatal("snapshot catalog unavailable within 2s", err)
	}
	if hasTool(listed.Tools, "ingress_hook") {
		t.Error("Claude catalog exposed ingress_hook")
	}
	t.Logf("initialize+catalog=%s", time.Since(start))
	return cs
}

func TestRunClientDiscoveryWhileParentUnresolved(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprint(native), func(t *testing.T) {
			parent := duplicateParent(t)
			parent.inspectClaude = func(ctx context.Context, _ int) (broker.AttachRequest, error) {
				<-ctx.Done()
				return broker.AttachRequest{}, ctx.Err()
			}
			socket := ""
			if native {
				socket = "/fixture/not-opened.sock"
			}
			t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", socket)
			startupClient(t, []string{"--broker-url", "http://127.0.0.1:1/mcp?project=test", "--token", "asb_p_synthetic", "--state-dir", t.TempDir()}, parent)
		})
	}
}

func TestDuplicateClaudeParentFilesSelectOwnAncestor(t *testing.T) {
	parent := duplicateParent(t)
	ancestors, _ := parent.ancestors()
	harness, pid, err := nearestAgent(ancestors, parent.classify)
	if err != nil || harness != "claude" || pid != 61530 {
		t.Fatal("wrong ancestor")
	}
	id, err := parent.inspectClaude(t.Context(), pid)
	if err != nil || id.PID != 61530 || id.SessionID != "11111111-1111-4111-8111-111111111111" {
		t.Fatal("duplicate conversation prevented parent selection", err)
	}
}

func TestRunClientDuplicateParentHeldLockKeepsDiscoveryResponsive(t *testing.T) {
	parent := duplicateParent(t)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "/fixture/not-opened.sock")
	dir := t.TempDir()
	state, err := clientstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	clientID, err := state.ClientID()
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://127.0.0.1:1/mcp?project=test"
	key := clientstate.SessionKey(brokerScope(endpoint, "test"), "claude", "11111111-1111-4111-8111-111111111111", clientID)
	unlock, err := state.LockSession(key)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	cs := startupClient(t, []string{"--broker-url", endpoint, "--token", "asb_p_synthetic", "--state-dir", dir}, parent)
	bound, cancel := context.WithTimeout(t.Context(), 7*time.Second)
	defer cancel()
	result, err := cs.CallTool(bound, &mcp.CallToolParams{Name: "auth_status", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	if !strings.Contains(string(raw), wantLocalLockMessage) || !strings.Contains(string(raw), "owned-local") {
		t.Fatal("missing owned-local guidance")
	}
	if _, err := state.LockSession(key); !errors.Is(err, clientstate.ErrLocked) {
		t.Fatal("competing client changed lock")
	}
}

func TestClaudeParentFileRejectsInvalidOrReusedProcess(t *testing.T) {
	valid := []byte(`{"pid":61530,"kind":"interactive","cwd":"/fixture","sessionId":"11111111-1111-4111-8111-111111111111"}`)
	for _, name := range []string{"pid", "uuid", "kind", "size", "reused", "dead"} {
		t.Run(name, func(t *testing.T) {
			data := valid
			switch name {
			case "pid":
				data = bytes.ReplaceAll(data, []byte("61530"), []byte("62251"))
			case "uuid":
				data = bytes.ReplaceAll(data, []byte("11111111-1111-4111-8111-111111111111"), []byte("invalid"))
			case "kind":
				data = bytes.ReplaceAll(data, []byte("interactive"), []byte("unknown"))
			case "size":
				data = bytes.Repeat([]byte(" "), claudeSessionFileLimit+1)
			}
			calls := 0
			_, err := claudeParentSessionFile(t.Context(), 61530, func(int) ([]byte, error) { return data, nil }, func(context.Context, int) (string, error) {
				calls++
				if name == "dead" {
					return "", errors.New("gone")
				}
				if name == "reused" && calls > 1 {
					return "new-process", nil
				}
				return "same-process", nil
			})
			if err == nil {
				t.Fatal("invalid identity accepted")
			}
		})
	}
}

// Credential failures are now reported after local discovery, rather than
// terminating the stdio process before the harness can initialize it.
func configuredRejection(t *testing.T, endpoint, token string) string {
	t.Helper()
	parent := duplicateParent(t)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "/fixture/not-opened.sock")
	cs := startupClient(t, []string{"--broker-url", endpoint + "/mcp?project=test", "--token", token, "--state-dir", t.TempDir()}, parent)
	bound, cancel := context.WithTimeout(t.Context(), 7*time.Second)
	defer cancel()
	// This explicit request triggers credential validation for a new conversation.
	_, _ = cs.CallTool(bound, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{"name": "fixture"}})
	r, err := cs.CallTool(bound, &mcp.CallToolParams{Name: "auth_status", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal("auth_status unavailable after rejection", err)
	}
	var status authStatus
	raw, _ := json.Marshal(r.StructuredContent)
	if json.Unmarshal(raw, &status) != nil || status.Status != authRejected {
		data, _ := json.Marshal(r)
		t.Fatalf("configured credential rejection missing: %s", data)
	}
	return status.Message
}
