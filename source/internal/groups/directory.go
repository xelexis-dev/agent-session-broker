package groups

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/pathguard"
	"os"
)

func newDirectoryPin(canonicalPath, operationPath string, identity os.FileInfo) (pathguard.Pin, error) {
	return pathguard.NewPin(canonicalPath, operationPath, identity, pathguard.PinLabels{Store: "membership", Access: "membership", Invalid: ErrInvalidInput})
}
