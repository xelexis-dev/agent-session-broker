package controlpanel

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

const wantSessionManageHelp = "세션 자신은 연결 해제만 할 수 있고, 다른 세션은 연결 해제·일시 중지·재개할 수 있습니다. Owner 토큰은 프로젝트의 모든 세션을, Developer 토큰은 같은 소유자의 세션만 관리합니다."

func TestSessionManageHelpInEveryIssuanceState(t *testing.T) {
	for _, tc := range []struct {
		name, user   string
		ready, error bool
	}{
		{"normal", "owner", true, false}, {"error", "owner", true, true}, {"off-admin", "admin", false, false}, {"off-owner", "owner", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := userStore(t)
			s.data.ScopeIssuanceReady = policyBool(tc.ready)
			h := New(s, tokenViewBackend{}, http.NotFoundHandler())
			client := loginPanelUser(t, h, tc.user)
			response := client.call("GET", "/projects/settings?project=demo", nil)
			if tc.error {
				response = client.call("POST", "/tokens/project", url.Values{"project": {"demo"}, "label": {"retained"}, "scope_selection": {"custom"}, "scope": {access.SessionRegister, access.SessionManage}, "auto_cap": {"100001"}})
			}
			wantStatus := 200
			if tc.error {
				wantStatus = 400
			}
			if response.Code != wantStatus {
				t.Fatalf("response=%d", response.Code)
			}
			body := response.Body.String()
			choice := regexp.MustCompile(`(?s)<label class="check"><input[^>]*value="session:manage".*?</label>`).FindString(body)
			want := `<code>session:manage</code></small><small data-scope-help="session:manage">` + wantSessionManageHelp + `</small>`
			if !strings.Contains(choice, want) {
				t.Errorf("manage checkbox lacks exact helper below scope code: %s", choice)
			}
			if (!tc.ready) != strings.Contains(body, `class="token-scope-options" disabled`) {
				t.Error("issuance gate changed")
			}
			if !strings.Contains(body, `>세션 감사</a>`) {
				t.Error("audit link not renamed")
			}
		})
	}
}

func TestSessionAuditTitlesAndEmptyCopy(t *testing.T) {
	s := userStore(t)
	s.data.ScopeIssuanceReady = true
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	owner := loginPanelUser(t, h, "owner")
	response := owner.call("GET", "/projects/audit?project=demo", nil)
	body := response.Body.String()
	for _, want := range []string{"<title>demo · 세션 감사 · ASB · Control Panel</title>", "<h1>demo 세션 감사</h1>", `id="direct-audit-title">세션 감사</h2>`, "표시할 세션 감사 이력이 없습니다."} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(body, "자동 등록 감사") || strings.Contains(body, "표시할 자동 등록 이력이 없습니다.") {
		t.Error("old audit title remains")
	}
	s.data.ScopeIssuanceReady = false
	off := owner.call("GET", "/projects/audit?project=demo", nil).Body.String()
	if !strings.Contains(off, "자동 등록이 활성화되지 않았습니다.") {
		t.Error("audit enable gate changed")
	}
}
