package channel

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

const claudeSessionFileLimit = 64 * 1024

// readClaudeSessionFile reads only Claude's small process roster entry, never
// transcripts or broker state. Missing/invalid entries fall back to observation.
func readClaudeSessionFile(pid int) ([]byte, error) {
	root := localHarnessHome("claude")
	if root == "" {
		return nil, os.ErrInvalid
	}
	file, err := os.Open(filepath.Join(root, "sessions", fmt.Sprintf("%d.json", pid)))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, claudeSessionFileLimit+1))
}

// Caller holds f.mu. This is a hint for an unchanged conversation only: any
// change or due startup retry retains the full stability and process-key checks.
func (f *claudeFollow) unchangedSession(id broker.AttachRequest) bool {
	if f.retryID == id.SessionID && !followPollClock(f.opts).now().Before(f.retryAt) {
		return false
	}
	return f.sessionFileMatches(id.SessionID)
}

// Caller holds f.mu. Both unchanged bindings and refused targets use the same
// bounded file read and parent validation before skipping CLI observation.
func (f *claudeFollow) sessionFileMatches(sessionID string) bool {
	read := f.opts.claudeSessionFile
	if read == nil {
		read = readClaudeSessionFile
	}
	data, err := read(f.initial.PID)
	if err != nil || len(data) > claudeSessionFileLimit {
		return false
	}
	var entry struct {
		PID       int    `json:"pid"`
		Kind      string `json:"kind"`
		CWD       string `json:"cwd"`
		SessionID string `json:"sessionId"`
	}
	return json.Unmarshal(data, &entry) == nil && entry.PID == f.initial.PID && (entry.Kind == "interactive" || entry.Kind == "bg") && sameCWD(entry.CWD, f.initial.CWD) && entry.SessionID != "" && entry.SessionID == sessionID
}
