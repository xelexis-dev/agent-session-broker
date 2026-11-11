package channel

import (
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func (o durableOptions) deviceCause() string {
	if identity, ok := o.receiverIdentity.(interface{ DiagnosticCause() string }); ok {
		return identity.DiagnosticCause()
	}
	return ""
}
func (o durableOptions) recordDeviceIdentity(key string, id broker.AttachRequest) {
	if cause := o.deviceCause(); cause != "" && o.state != nil && key != "" {
		_ = o.state.RecordDiagnostic(key, clientstate.DiagnosticEvent{At: time.Now().UTC(), SessionID: id.SessionID, Event: "device-identity", Reason: cause})
	}
}
func (o durableOptions) diagnoseDeviceIdentity(b *ingressBinding) {
	b.mu.Lock()
	id := b.identity
	key := ""
	if b.durable != nil {
		key = b.durable.key
	}
	b.mu.Unlock()
	o.recordDeviceIdentity(key, id)
}
func (o durableOptions) deviceIdentityStatus(out authStatus) authStatus {
	code := o.deviceCause()
	if code == "" {
		return out
	}
	hint := ""
	switch code {
	case "ok":
		hint = "Device identity is available."
	case "path-unavailable":
		hint = "Check the environment variables for the device identity path."
	case "directory-insecure":
		hint = "Check the device identity directory's owner, permissions and links."
	case "directory-unwritable":
		hint = "Check write permissions so the device identity directory can be created."
	case "lock-failed":
		hint = "Check the device identity lock file's permissions and try again."
	case "secret-insecure":
		hint = "Check the device secret file's owner, permissions and links."
	case "secret-malformed":
		hint = "The device secret has an invalid format. Confirm whether to reset it to a new device identity."
	case "secret-missing":
		hint = "The device secret is missing. Restart the client."
	case "secret-changed":
		hint = "The device secret changed. Restart the client."
	case "secret-create-failed":
		hint = "Could not create the device secret. Check disk space and permissions."
	case "process-identity-unavailable":
		hint = "Could not verify the process identity. Try again."
	case "secret-created":
		hint = "Created a new device secret."
	case "secret-created-lock-present":
		hint = "Created a new device secret while a lock file remained. The previous secret history is unknown."
	default:
		code = "unknown"
		hint = "Unknown device identity state. Check the client version."
	}
	out.DeviceIdentity, out.DeviceIdentityHint = code, hint
	return out
}
