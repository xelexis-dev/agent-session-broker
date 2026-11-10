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
		hint = "장치 식별을 사용할 수 있습니다."
	case "path-unavailable":
		hint = "장치 식별 경로의 환경 변수를 확인하세요."
	case "directory-insecure":
		hint = "장치 식별 폴더의 소유자·권한·링크를 확인하세요."
	case "directory-unwritable":
		hint = "장치 식별 폴더를 만들 수 있도록 쓰기 권한을 확인하세요."
	case "lock-failed":
		hint = "장치 식별 잠금 파일의 권한을 확인하고 다시 시도하세요."
	case "secret-insecure":
		hint = "장치 비밀값 파일의 소유자·권한·링크를 확인하세요."
	case "secret-malformed":
		hint = "장치 비밀값의 형식이 잘못되었습니다. 새 장치 식별로 초기화할지 확인하세요."
	case "secret-missing":
		hint = "장치 비밀값이 없습니다. client를 재시작하세요."
	case "secret-changed":
		hint = "장치 비밀값이 바뀌었습니다. client를 재시작하세요."
	case "secret-create-failed":
		hint = "장치 비밀값을 만들지 못했습니다. 디스크 공간과 권한을 확인하세요."
	case "process-identity-unavailable":
		hint = "프로세스 식별을 확인할 수 없습니다. 다시 시도하세요."
	case "secret-created":
		hint = "새 장치 비밀값을 만들었습니다."
	case "secret-created-lock-present":
		hint = "잠금 파일이 남아 있는 상태에서 새로 만들었습니다. 이전 비밀값 이력은 알 수 없습니다"
	default:
		code = "unknown"
		hint = "알 수 없는 장치 식별 상태입니다. client 버전을 확인하세요."
	}
	out.DeviceIdentity, out.DeviceIdentityHint = code, hint
	return out
}
