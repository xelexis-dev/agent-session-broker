package channel

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

const wantLocalLockMessage = "This session is held by another local client process; it is released when that process exits."

func TestSecondClientLocalLockDiagnostics(t *testing.T) {
	auth := newDeviceAuthServer(t)
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clientID, err := state.ClientID()
	if err != nil {
		t.Fatal(err)
	}
	id := broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 4242, CWD: "/fixture"}
	key := clientstate.SessionKey("https://broker/mcp?project=brk", id.Harness, id.SessionID, clientID)
	release, err := state.LockSession(key)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	lockPath := filepath.Join(state.Dir(), "sessions", key, "conn.lock")
	before, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	second := newDeviceModeFixture(t, auth, newSwitchableUpstream(t), state.Dir(), map[string]broker.AttachRequest{"thread": id})
	for _, tool := range []string{"list_sessions", "attach_session"} {
		result := second.call("thread", tool, map[string]any{})
		raw, _ := json.Marshal(result)
		if !result.IsError || !strings.Contains(string(raw), wantLocalLockMessage) {
			t.Errorf("%s lock guidance = %s", tool, raw)
		}
	}
	statusResult := second.call("thread", "auth_status", map[string]any{})
	raw, _ := json.Marshal(statusResult.StructuredContent)
	var status authStatus
	if json.Unmarshal(raw, &status) != nil || statusResult.IsError || status.Registered || status.Message != wantLocalLockMessage || status.ReceiveError != "owned-local" {
		t.Errorf("lock auth_status = %s", raw)
	}
	diagnostics, err := state.LoadDiagnostics(key)
	if err != nil || !hasDiagnostic(diagnostics, "reconnect-failed", "owned-local") || diagnostics.Summary.LastFailureCode != "owned-local" {
		t.Errorf("local ownership diagnostic missing: %+v, %v", diagnostics, err)
	}
	if status.Diagnostics == nil || status.Diagnostics.LastFailureCode != "owned-local" {
		t.Error("auth_status missing owned-local summary")
	}
	if _, err := state.LockSession(key); !errors.Is(err, clientstate.ErrLocked) {
		t.Fatalf("lock was released: %v", err)
	}
	after, err := os.Stat(lockPath)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("lock file was replaced")
	}
	if second.codeRequests() != 0 {
		t.Fatal("competing client started authorization")
	}
}

func TestLocalLockFailurePreservesErrorIdentity(t *testing.T) {
	for _, device := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "project"}[device], func(t *testing.T) {
			state, err := clientstate.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			opts := durableOptions{state: state, scope: "fixture"}
			if device {
				opts.device = &deviceAuthOptions{}
			}
			id := broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111"}
			failure := opts.localLockFailure(id, clientstate.ErrLocked)
			if !errors.Is(failure, clientstate.ErrLocked) || failure.Error() != wantLocalLockMessage {
				t.Fatalf("lost lock identity: %v", failure)
			}
			remote := errors.New("owned")
			if opts.localLockFailure(id, remote) != remote {
				t.Fatal("remote ownership was classified as local")
			}
		})
	}
}
