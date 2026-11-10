package access

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestScopeCatalogMatchesREADME(t *testing.T) {
	rows := ScopeCatalog()
	if len(rows) != 9 {
		t.Fatal("scope catalog must contain nine entries")
	}
	scopes := []string{}
	for _, d := range rows {
		scopes = append(scopes, d.Scope)
	}
	if !ValidScopes(scopes) {
		t.Fatal("catalog is not a canonical complete scope set")
	}
	raw, e := os.ReadFile("../../../README_AGENT.md")
	if e != nil {
		t.Fatal(e)
	}
	for _, d := range rows {
		want := "| `" + d.Scope + "` | " + d.Label + " | " + d.Description + " |"
		if strings.Count(string(raw), want) != 1 {
			t.Errorf("README catalog row mismatch for %s", d.Scope)
		}
	}
	rows[0].Label = "changed"
	if ScopeCatalog()[0].Label == "changed" {
		t.Fatal("catalog returned shared state")
	}
	if !slices.Equal(DelegatedScopes(scopes), LegacyV1Scopes()) {
		t.Fatal("catalog adds or drops legacy permissions")
	}
}

func TestAutoRegistrationScopeExplainsExplicitConnectionRequest(t *testing.T) {
	detail := ScopeDetails([]string{SessionRegisterAuto})[0]
	if detail.Label != "연결 요청 시 승인 없이 등록" {
		t.Errorf("label = %q", detail.Label)
	}
	for _, want := range []string{"사용자가 이 대화의 연결을 요청하면 승인 절차 없이 등록합니다.", "새 대화가 시작되는 것만으로 등록하지 않습니다.", "프로젝트 토큰으로 사람의 승인 없이 직접 등록", "세션 자격증명에는 위임되지 않습니다."} {
		if !strings.Contains(detail.Description, want) {
			t.Errorf("auto scope explanation missing %q", want)
		}
	}
}

func TestSessionManageScopeExplainsOtherSessionControl(t *testing.T) {
	const want = "stop_session으로 이 세션의 연결을 해제하며, stop_session·retire_session·suspend_session·unsuspend_session으로 토큰 소유자가 관리할 수 있는 다른 세션의 연결을 해제·일시 중지·재개합니다. retire_session은 등록과 사용한 세션 자격을 회수해 이름 예약을 해제합니다. 자기 retire와 이 세션의 일시 중지·재개는 허용되지 않습니다. 활성 인스턴스 관리자 소유 토큰은 list_bug_reports·get_bug_report·set_bug_report_status로 현재 프로젝트 세대의 버그 리포트를 조회·처리할 수 있습니다."
	got := ScopeDetails([]string{SessionManage})[0]
	if got.Description != want {
		t.Errorf("session:manage description = %q", got.Description)
	}
}
