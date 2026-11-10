//go:build darwin

package codexcontrol

import "github.com/xelexis-dev/agent-session-broker/source/internal/processinfo"

func processKeyForTest(pid int) (string, error) {
	info, err := processinfo.Read(pid)
	return info.Key, err
}
