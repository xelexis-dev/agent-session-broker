package channel

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/localactivity"
	"os"
	"path/filepath"
)

// Invalid explicit roots must not observe a different installation via fallback.
func localHarnessHome(harness string) string {
	var key, leaf string
	switch harness {
	case "codex":
		key, leaf = "CODEX_HOME", ".codex"
	case "claude":
		key, leaf = "CLAUDE_CONFIG_DIR", ".claude"
	default:
		return ""
	}
	root := os.Getenv(key)
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = localactivity.HomeRoot(home)
		if home == "" {
			return ""
		}
		root = filepath.Join(home, leaf)
	}
	return localactivity.HomeRoot(root)
}
