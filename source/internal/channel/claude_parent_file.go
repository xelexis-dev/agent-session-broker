package channel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// Select only the exact process ancestor's bounded roster file. Another live
// process may share its sessionId; that never makes it this client's parent.
func claudeParentSessionFile(ctx context.Context, pid int, read func(int) ([]byte, error), key func(context.Context, int) (string, error)) (broker.AttachRequest, error) {
	fail := func() (broker.AttachRequest, error) {
		return broker.AttachRequest{}, errors.New("current Claude parent identity is unavailable")
	}
	if ctx.Err() != nil || pid <= 0 {
		return fail()
	}
	before, err := key(ctx, pid)
	if err != nil || before == "" {
		return fail()
	}
	data, err := read(pid)
	if err != nil || len(data) > claudeSessionFileLimit {
		return fail()
	}
	var entry struct {
		PID       int    `json:"pid"`
		Kind      string `json:"kind"`
		CWD       string `json:"cwd"`
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(data, &entry) != nil || entry.PID != pid || (entry.Kind != "interactive" && entry.Kind != "bg") || !parentSessionIDPattern.MatchString(entry.SessionID) || strings.TrimSpace(entry.CWD) == "" || strings.ContainsRune(entry.CWD, 0) {
		return fail()
	}
	after, err := key(ctx, pid)
	if err != nil || after != before || ctx.Err() != nil {
		return fail()
	}
	return broker.AttachRequest{Harness: "claude", PID: pid, SessionID: entry.SessionID, CWD: entry.CWD}, nil
}
