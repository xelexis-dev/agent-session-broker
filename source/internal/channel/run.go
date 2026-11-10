package channel

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/claude"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type rosterRow struct {
	SessionID string `json:"sessionId"`
	PID       int    `json:"pid"`
	Kind      string `json:"kind"`
}

// parentIdentity binds this client to the nearest Claude ancestor only. A
// new session appears in the roster a moment after its MCP clients start, so
// that row is awaited; an outer interactive Claude session in the same
// process tree (a terminal or tool opened by Claude) never qualifies.
func parentIdentity(ctx context.Context) (broker.AttachRequest, error) {
	ancestors, err := ancestorPIDs()
	if err != nil {
		return broker.AttachRequest{}, err
	}
	harness, pid, err := nearestAgent(ancestors, processHarness)
	if err != nil {
		return broker.AttachRequest{}, err
	}
	if harness != "claude" {
		return broker.AttachRequest{}, errors.New("nearest parent agent is not a Claude session")
	}
	return claudeParentIdentity(ctx, &claude.CLIExecutor{}, pid)
}

func claudeParentIdentity(ctx context.Context, executor claude.Executor, pid int) (broker.AttachRequest, error) {
	if id, err := claudeParentSessionFile(ctx, pid, readClaudeSessionFile, claudeParentProcessKey); err == nil {
		return id, nil
	}
	roster := func(ctx context.Context) ([]rosterRow, error) {
		raw, err := executor.Run(ctx, claude.Call{Args: []string{"agents", "--json", "--all"}})
		if err != nil {
			return nil, err
		}
		var rows []rosterRow
		if err := json.Unmarshal([]byte(raw), &rows); err != nil {
			return nil, err
		}
		return rows, nil
	}
	inspect := func(ctx context.Context, _ string) (broker.AttachRequest, error) {
		return inspectClaudeParent(ctx, executor, pid)
	}
	return nearestClaudeIdentity(ctx, pid, roster, inspect, 15*time.Second, 250*time.Millisecond)
}

// nearestClaudeIdentity polls the roster until the unique client-parent row of exactly
// parentPID can be inspected, or the deadline passes.
func nearestClaudeIdentity(ctx context.Context, parentPID int, roster func(context.Context) ([]rosterRow, error), inspect func(context.Context, string) (broker.AttachRequest, error), deadline, tick time.Duration) (broker.AttachRequest, error) {
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		rows, err := roster(probeCtx)
		if err == nil {
			sessionID, valid := parentRosterSessionID(rows, parentPID)
			if valid {
				identity, inspectErr := inspect(probeCtx, sessionID)
				if inspectErr == nil && identity.Harness == "claude" && identity.PID == parentPID && identity.SessionID == sessionID {
					cancel()
					return identity, nil
				}
			}
		}
		cancel()
		select {
		case <-ctx.Done():
			return broker.AttachRequest{}, ctx.Err()
		case <-timer.C:
			return broker.AttachRequest{}, errors.New("channel could not identify its exact parent Claude session")
		case <-ticker.C:
		}
	}
}

var parentSessionIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func parentRosterSessionID(rows []rosterRow, pid int) (string, bool) {
	var parent *rosterRow
	for i := range rows {
		if rows[i].PID != pid {
			continue
		}
		if parent != nil {
			return "", false
		}
		parent = &rows[i]
	}
	if parent == nil || (parent.Kind != "interactive" && parent.Kind != "background") || !parentSessionIDPattern.MatchString(parent.SessionID) {
		return "", false
	}
	return parent.SessionID, true
}

func inspectClaudeParent(ctx context.Context, executor claude.Executor, pid int) (broker.AttachRequest, error) {
	identity, err := claude.NewAdapter(executor).InspectParent(ctx, pid)
	if err != nil {
		return broker.AttachRequest{}, err
	}
	return broker.AttachRequest{Harness: "claude", SessionID: identity.ID, PID: identity.PID, CWD: identity.CWD}, nil
}
