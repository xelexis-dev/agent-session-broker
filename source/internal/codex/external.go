package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

var externalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// InspectExternal observes local metadata and a live Codex process. The caller
// supplies their association; this local attachment is not caller authentication.
func (a *Adapter) InspectExternal(ctx context.Context, request broker.AttachRequest) (broker.ClaudeIdentity, string, error) {
	missing := func() (broker.ClaudeIdentity, string, error) { return broker.ClaudeIdentity{}, "", ErrSessionNotFound }
	if !externalUUID.MatchString(request.SessionID) || request.PID <= 0 || request.Name == "" || !filepath.IsAbs(request.CWD) {
		return missing()
	}
	metaCWD, err := a.externalSessionCWD(request.SessionID)
	if err != nil {
		return missing()
	}
	sameCWD := filepath.Clean(metaCWD) == filepath.Clean(request.CWD)
	if runtime.GOOS == "windows" {
		sameCWD = strings.EqualFold(filepath.Clean(metaCWD), filepath.Clean(request.CWD))
	}
	if !sameCWD {
		return missing()
	}
	probe := a.externalProbe
	if probe == nil {
		probe = externalProcessKey
	}
	key, err := probe(ctx, request.PID)
	if err != nil || key == "" {
		return missing()
	}
	return broker.ClaudeIdentity{ID: request.SessionID, ConversationID: request.SessionID, Name: request.Name, CWD: metaCWD, PID: request.PID, Status: broker.StatusIdle}, key, nil
}

// ObserveExternal obtains cwd from the exact local thread metadata. The caller
// supplies an observed ancestor PID, never a PID chosen by a remote MCP caller.
func (a *Adapter) ObserveExternal(ctx context.Context, id string, pid int) (broker.AttachRequest, error) {
	cwd, err := a.externalSessionCWD(id)
	if err != nil {
		return broker.AttachRequest{}, err
	}
	r := broker.AttachRequest{Harness: "codex", Name: "identity-probe", SessionID: id, PID: pid, CWD: cwd}
	if _, _, err := a.InspectExternal(ctx, r); err != nil {
		return broker.AttachRequest{}, err
	}
	r.Name = ""
	return r, nil
}

func (a *Adapter) externalSessionCWD(id string) (string, error) {
	missing := func() (string, error) { return "", ErrSessionNotFound }
	if !externalUUID.MatchString(id) {
		return missing()
	}
	root := a.externalHome
	if root == "" {
		root = os.Getenv("CODEX_HOME")
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return missing()
		}
		root = filepath.Join(home, ".codex")
	}
	paths, err := filepath.Glob(filepath.Join(root, "sessions", "*", "*", "*", "rollout-*-"+id+".jsonl"))
	if err != nil || len(paths) != 1 {
		return missing()
	}
	file, err := os.Open(paths[0])
	if err != nil {
		return missing()
	}
	defer file.Close()
	reader := bufio.NewReader(io.LimitReader(file, 1024*1024))
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return missing()
	}
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			ID  string `json:"id"`
			CWD string `json:"cwd"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &meta) != nil || meta.Type != "session_meta" || meta.Payload.ID != id || !filepath.IsAbs(meta.Payload.CWD) {
		return missing()
	}
	return meta.Payload.CWD, nil
}
