package controlpanel

import (
	"crypto/rand"
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"net/http"
	"time"
)

type pendingNotice struct {
	code, target string
	reportID     string
	expires      time.Time
}

const tokenScopeUpgradeMessage = "제한 권한·자동 등록 토큰 발급이 인스턴스 설정에서 꺼져 있습니다. 관리자에게 문의하세요. 제한 권한은 클라이언트 0.15 이상과 호환성 검증 완료가 필요합니다. 지금은 기존 전체 권한으로 발급해 주세요."
const tokenScopeInvalidMessage = "토큰 권한과 자동 등록 설정을 확인해 주세요. session:register는 필수이며, 자동 등록 한도는 0(무제한) 또는 1~100000개, 수명은 0(무기한) 또는 1~36500일입니다."

const tokenCapMessage = "프로젝트의 활성 토큰 한도(256개)에 도달했습니다. 사용하지 않는 토큰을 취소하거나 오래된 세션을 해제한 뒤 다시 발급·승인해 주세요."

var noticeMessages = map[string]string{
	"bug-report-deleted":           "리포트를 삭제했습니다.",
	"bug-report-delete-incomplete": bugreport.ErrDeleteIncomplete.Error() + ".",
	"bug-report-resolved":          "상태를 해결로 바꿨습니다.",
	"bug-report-ignored":           "상태를 무시로 바꿨습니다.",
	"bug-report-open":              "상태를 열림으로 바꿨습니다.",
	"token-scopes-saved":           "토큰 권한을 변경했습니다.",
	"session-grant-released":       "닉네임 예약을 해제했습니다. 같은 닉네임으로 다시 승인할 수 있습니다.",
	"session-suspended":            "세션을 일시 중지하고 대기 메시지를 폐기했습니다.",
	"session-unsuspended":          "일시 중지를 해제했습니다. 클라이언트가 다시 연결되면 새 메시지를 받을 수 있습니다.",
	"user-saved-relogin":           "사용자 정보를 저장했습니다. 변경된 계정 정보로 다시 로그인해 주세요.",
	"project-created":              "프로젝트를 추가했습니다.", "project-saved": "프로젝트 설정을 저장했습니다.",
	"instance-http-policy-saved": "HTTP 연결 정책을 저장했습니다.",
	"members-unchanged":          "변경된 멤버 정보가 없습니다.",
	"members-saved":              "프로젝트 멤버 정보를 변경했습니다.", "token-revoked": "토큰을 취소했습니다.",
	"session-detached": "세션 연결을 해제했습니다.", "user-created": "사용자를 추가했습니다.",
	"user-saved": "사용자 정보를 저장했습니다.", "profile-saved": "프로필 정보를 저장했습니다.",
	"password-saved":  "비밀번호를 변경했습니다. 새 비밀번호로 다시 로그인해 주세요.",
	"setup-saved":     "관리자 비밀번호를 설정했습니다. 로그인해 주세요.",
	"project-deleted": "프로젝트와 관련 데이터를 삭제했습니다.",
	"device-approved": "세션을 승인했습니다. 클라이언트가 곧 등록됩니다.",
	"device-denied":   "세션 승인 요청을 거절했습니다.",
}

// A random, single-use cookie refers to a fixed server-side message. It conveys
// no authorization and contains neither form values nor credentials.
func (p *panel) redirectNotice(w http.ResponseWriter, r *http.Request, target, code string, reportID ...string) {
	if noticeMessages[code] != "" {
		now := time.Now()
		key := rand.Text()
		p.mu.Lock()
		if p.notices == nil {
			p.notices = map[string]pendingNotice{}
		}
		for id, n := range p.notices {
			if !now.Before(n.expires) {
				delete(p.notices, id)
			}
		}
		// Bound transient storage independently of user actions or abandoned tabs.
		if len(p.notices) >= 256 {
			for id := range p.notices {
				delete(p.notices, id)
				break
			}
		}
		notice := pendingNotice{code: code, target: target, expires: now.Add(2 * time.Minute)}
		if code == "bug-report-deleted" && len(reportID) == 1 {
			notice.reportID = reportID[0]
		}
		p.notices[key] = notice
		p.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "asb_notice", Value: key, Path: "/", MaxAge: 120, HttpOnly: true, Secure: p.secure(r), SameSite: http.SameSiteStrictMode})
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (p *panel) takeNotice(w http.ResponseWriter, r *http.Request) string {
	cookie, err := r.Cookie("asb_notice")
	if err != nil {
		return ""
	}
	p.mu.Lock()
	n, ok := p.notices[cookie.Value]
	if ok && time.Now().Before(n.expires) && (r.Method != http.MethodGet || r.URL.RequestURI() != n.target) {
		p.mu.Unlock()
		return ""
	}
	delete(p.notices, cookie.Value)
	p.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "asb_notice", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: p.secure(r), SameSite: http.SameSiteStrictMode})
	if !ok || !time.Now().Before(n.expires) {
		return ""
	}
	if n.code == "bug-report-deleted" && n.reportID != "" {
		return fmt.Sprintf("리포트 %s를 삭제했습니다.", n.reportID)
	}
	return noticeMessages[n.code]
}
