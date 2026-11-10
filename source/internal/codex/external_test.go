package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

func TestExternalMetadataAndProcessMustBothMatch(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	id := "01a070ed-0c7f-71b0-a10e-885c3a8e89b8"
	directory := filepath.Join(root, "sessions", "2026", "09", "05")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "cwd": cwd}})
	if err := os.WriteFile(filepath.Join(directory, "rollout-date-"+id+".jsonl"), append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	a := NewAdapter()
	a.externalHome = root
	a.externalProbe = func(context.Context, int) (string, error) { return "pinned-creation", nil }
	request := broker.AttachRequest{Harness: "codex", Name: "current", SessionID: id, PID: 123, CWD: cwd}
	got, key, err := a.InspectExternal(context.Background(), request)
	if err != nil || got.ID != id || key != "pinned-creation" {
		t.Fatalf("inspect: %#v %s %v", got, key, err)
	}
	request.CWD = t.TempDir()
	if _, _, err := a.InspectExternal(context.Background(), request); err == nil {
		t.Fatal("accepted wrong cwd")
	}
	request.CWD = cwd
	a.externalProbe = func(context.Context, int) (string, error) { return "", ErrSessionNotFound }
	if _, _, err := a.InspectExternal(context.Background(), request); err == nil {
		t.Fatal("accepted dead process")
	}
	request.SessionID = "../../escape"
	if _, _, err := a.InspectExternal(context.Background(), request); err == nil {
		t.Fatal("accepted invalid identity")
	}
}
