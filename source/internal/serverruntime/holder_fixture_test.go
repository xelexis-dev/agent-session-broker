package serverruntime

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"strings"
)

// These fixtures execute successive operations in one Go receiver process.
// Explicit holder-competition/legacy tests supply distinct or absent metadata.
func g4FixtureHolder() *protocol.ReceiverHolder {
	return &protocol.ReceiverHolder{PID: 1, DeviceID: strings.Repeat("f", 64), Incarnation: strings.Repeat("e", 64)}
}
