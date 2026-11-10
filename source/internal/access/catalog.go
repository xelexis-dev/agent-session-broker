package access

import "time"

// ScopeDetail is shared by token issuance and credential diagnostics.
type ScopeDetail struct {
	Scope       string `json:"scope"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// ScopeCatalog returns a copy in canonical scope order. Descriptions describe
// permission checks; identity/ownership checks still apply independently.
func ScopeCatalog() []ScopeDetail {
	return []ScopeDetail{
		{MemoryRead, "메모리 조회", "memory_get과 memory_search로 프로젝트 메모리를 조회합니다."},
		{MemoryWrite, "메모리 쓰기", "memory_put으로 프로젝트 메모리를 저장합니다."},
		{MessageReceive, "메시지 수신", "wait_for_message로 자기 세션에 도착한 메시지를 수신합니다."},
		{MessageSend, "메시지 발송", "send_message로 등록된 자기 세션에서 다른 세션으로 메시지를 보냅니다."},
		{SessionManage, "세션 관리", "stop_session으로 이 세션의 연결을 해제하며, stop_session·retire_session·suspend_session·unsuspend_session으로 토큰 소유자가 관리할 수 있는 다른 세션의 연결을 해제·일시 중지·재개합니다. retire_session은 등록과 사용한 세션 자격을 회수해 이름 예약을 해제합니다. 자기 retire와 이 세션의 일시 중지·재개는 허용되지 않습니다. 활성 인스턴스 관리자 소유 토큰은 list_bug_reports·get_bug_report·set_bug_report_status로 현재 프로젝트 세대의 버그 리포트를 조회·처리할 수 있습니다."},
		{SessionRead, "세션 조회", "list_sessions와 get_session으로 프로젝트 세션을 조회합니다."},
		{SessionRegister, "등록·연결 유지 (필수)", "attach_session·register_session·resume_session·renew_lease·probe_response·report_ingress_failure로 자기 세션을 등록하고 연결을 유지합니다."},
		{SessionRegisterAuto, "연결 요청 시 승인 없이 등록", "사용자가 이 대화의 연결을 요청하면 승인 절차 없이 등록합니다. 새 대화가 시작되는 것만으로 등록하지 않습니다. 프로젝트 토큰으로 사람의 승인 없이 직접 등록을 요청하며 세션 자격증명에는 위임되지 않습니다."},
		{SessionRename, "자기 이름 변경", "rename_session으로 자기 세션의 통신 이름을 변경합니다."},
	}
}

func ScopeDetails(scopes []string) []ScopeDetail {
	out := make([]ScopeDetail, 0, len(scopes))
	for _, d := range ScopeCatalog() {
		if HasScope(scopes, d.Scope) {
			out = append(out, d)
		}
	}
	return out
}

// TokenInfoTool is callable for diagnostics but never advertised to a host.
const TokenInfoTool = "session_token_info"

type TokenInfo struct {
	ProjectGenerationID string    `json:"projectGenerationId,omitempty"`
	ProjectRefusals     bool      `json:"projectRefusals,omitempty"`
	Scopes              []string  `json:"scopes"`
	AdmissionMode       string    `json:"admissionMode"`
	ExpiresAt           time.Time `json:"expiresAt,omitzero"`
}
