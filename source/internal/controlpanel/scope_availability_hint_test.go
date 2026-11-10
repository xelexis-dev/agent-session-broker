package controlpanel

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestScopeAvailabilityHintByRoleAndReadiness(t *testing.T) {
	s := userStore(t)
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	for _, tc := range []struct {
		name, user string
		ready      bool
	}{
		{"admin-off", "admin", false}, {"owner-off", "owner", false},
		{"admin-on", "admin", true}, {"owner-on", "owner", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.data.ScopeIssuanceReady = policyBool(tc.ready)
			response := loginPanelUser(t, h, tc.user).call("GET", "/projects/settings?project=demo", nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d", response.Code)
			}
			body := response.Body.String()
			fieldset := regexp.MustCompile(`(?s)<fieldset class="token-scope-options"[^>]*>.*?</fieldset>`).FindString(body)
			if !strings.Contains(fieldset, `aria-describedby="scope-availability"`) {
				t.Error("scope fieldset lacks availability description")
			}
			if strings.Contains(body, "LEGACY_V1") || strings.Contains(body, "인스턴스 설정에서 제한 권한·자동 등록 토큰 발급이 꺼져 있습니다") {
				t.Error("old long notice exposed")
			}
			hint := regexp.MustCompile(`<p id="scope-availability" class="form-hint">(.*?)</p>`).FindStringSubmatch(fieldset)
			if len(hint) != 2 {
				t.Fatal("scope availability hint missing")
			}
			ready := "발급 후 일반 권한은 토큰 목록의 '권한 변경'에서 바꿀 수 있습니다. 승인 방식·자동 등록 한도·수명은 바꿀 수 없습니다."
			admin := `제한 권한 발급이 꺼져 있습니다. <a href="/admin/settings">인스턴스 설정</a>에서 켤 수 있습니다.`
			owner := "제한 권한 발급이 꺼져 있습니다. 인스턴스 관리자에게 요청하세요."
			want := owner
			if tc.ready {
				want = ready
			} else if tc.user == "admin" {
				want = admin
			}
			if hint[1] != want {
				t.Errorf("hint=%q, want %q", hint[1], want)
			}
			if tc.ready && strings.Contains(fieldset, "제한 권한 발급이 꺼져 있습니다.") {
				t.Error("off hint shown while ready")
			}
			header, _, _ := strings.Cut(fieldset, ">")
			if (!tc.ready) != strings.Contains(header, " disabled") {
				t.Error("scope gate disabled state changed")
			}
		})
	}
}
