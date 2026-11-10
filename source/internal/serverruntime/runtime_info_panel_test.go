package serverruntime

import (
	"context"
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/localactivity"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
)

func TestBoundCodexModelEffortReachesPanelLiveData(t *testing.T) {
	store := protectedStore(t)
	remote := broker.NewRemoteService()
	registry, err := groups.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	factory := mcpserver.NewManagedProjectFactory(registry, func(groups.ProjectDirectory) (mcpserver.Backend, error) {
		return newProtectedBackend("project", remote), nil
	})
	defer factory.Close()
	backend, err := factory.RemoteBackend(t.Context(), "project")
	if err != nil {
		t.Fatal(err)
	}
	grant, _ := protectedGrant(t, store, "model-worker")
	ctx := access.WithGrant(t.Context(), grant)
	req := protectedRequest(1, "ignored")
	req.CWD = t.TempDir()
	registered, err := backend.(*protectedBackend).RegisterSession(ctx, req)
	if err != nil || registered.Code != "" {
		t.Fatal("registration failed")
	}
	home := t.TempDir()
	path := filepath.Join(home, "sessions", "2026", "10", "14", "rollout-fixture-"+req.SessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": req.SessionID, "cwd": req.CWD}})
	event, _ := json.Marshal(map[string]any{"type": "event_msg", "timestamp": now, "payload": map[string]any{"type": "task_started", "turn_id": "fixture-turn"}})
	turn, _ := json.Marshal(map[string]any{"type": "turn_context", "timestamp": now, "payload": map[string]any{"turn_id": "fixture-turn", "model": "gpt-6.1-sol", "effort": "medium"}})
	if err := os.WriteFile(path, []byte(string(meta)+"\n"+string(event)+"\n"+string(turn)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	binding := localactivity.Binding{Harness: "codex", SessionID: req.SessionID, ClientID: req.ClientID, RegistrationID: registered.Session.RegistrationID, LeaseID: req.ChannelID, Generation: registered.Session.Generation}
	observer := localactivity.New(localactivity.Config{Binding: binding, Home: home, CWD: req.CWD, PID: req.PID, Host: "fixture", Platform: "fixture", Format: localactivity.CodexTaskPairV1, Probe: func(context.Context, int) (localactivity.Process, error) {
		return localactivity.Process{Key: "process", Host: "fixture", Platform: "fixture"}, nil
	}})
	selected, _ := json.Marshal(observer.Observe(t.Context(), binding))
	var observed map[string]any
	_ = json.Unmarshal(selected, &observed)
	raw, _ := json.Marshal(map[string]any{"registrationId": binding.RegistrationID, "channelId": binding.LeaseID, "runtimeInfo": map[string]any{"model": observed["Model"], "effort": observed["Effort"], "sequence": 1, "generation": binding.Generation, "observedAt": now}})
	var renew broker.RenewRequest
	if err := json.Unmarshal(raw, &renew); err != nil {
		t.Fatal(err)
	}
	if out, err := backend.(*protectedBackend).RenewLease(ctx, renew); err != nil || out.Code != "" {
		t.Fatal("renew failed")
	}
	rows, err := (panelBackend{factory: factory}).LiveSessions(t.Context(), "project")
	if err != nil {
		t.Fatal(err)
	}
	panelJSON, _ := json.Marshal(rows)
	if !strings.Contains(string(panelJSON), "gpt-6.1-sol") || !strings.Contains(string(panelJSON), "medium") {
		t.Fatal("panel live data lacks bound Codex model / effort")
	}
	session, _ := remote.GetSession(t.Context(), registered.Session.RegistrationID)
	wire, _ := json.Marshal(session)
	if strings.Contains(string(wire), "gpt-6.1-sol") || strings.Contains(string(wire), "runtimeInfo") {
		t.Fatal("panel-only runtime info leaked into MCP Session")
	}
}

func TestRuntimeInfoAuthorizationAndRevocation(t *testing.T) {
	store := protectedStore(t)
	remote := broker.NewRemoteService()
	defer remote.Close()
	backend := newProtectedBackend("project", remote)
	owner, _ := protectedGrant(t, store, "runtime-owner")
	other, _ := protectedGrant(t, store, "runtime-other")
	req := protectedRequest(1, "ignored")
	ctx := access.WithGrant(t.Context(), owner)
	out, err := backend.RegisterSession(ctx, req)
	if err != nil || out.Code != "" {
		t.Fatal("register failed")
	}
	raw, _ := json.Marshal(protocol.RuntimeInfo{Model: "fixture-model", Effort: "high", Sequence: 1, Generation: out.Session.Generation, ObservedAt: time.Now().UTC()})
	renew := broker.RenewRequest{RegistrationID: out.Session.RegistrationID, ChannelID: req.ChannelID, RuntimeInfo: raw}
	if result, err := backend.RenewLease(ctx, renew); err != nil || result.Code != "" {
		t.Fatal("renew failed")
	}
	for _, grant := range []access.Grant{other, func() access.Grant { g := owner; g.Project = "foreign"; return g }()} {
		if result, err := backend.RenewLease(access.WithGrant(t.Context(), grant), renew); err == nil && result.Code == "" {
			t.Fatal("foreign grant accepted")
		}
	}
	if len(remote.SessionRuntimeInfo()) != 1 {
		t.Fatal("foreign grant changed owner snapshot")
	}
	if err := store.RevokeToken("project", owner.TokenID); err != nil {
		t.Fatal(err)
	}
	if len(remote.SessionRuntimeInfo()) != 0 {
		t.Fatal("revoked grant left model visible")
	}
}
