package controlpanel

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
)

var wantSessionManageHelp = en("tokens.scope_manage_help")

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
			if !strings.Contains(body, (">" + en("tokens.session_audit") + "</a>")) {
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
	for _, want := range []string{"<title>demo · " + en("title.audit") + " · ASB · Control Panel</title>", "<h1>" + en("heading.project_audit", "project", "demo") + "</h1>", `id="direct-audit-title">` + en("title.audit") + `</h2>`, enHTML("audit.empty")} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	useKorean(t, s)
	korean := owner.call("GET", "/projects/audit?project=demo", nil).Body.String()
	if !strings.Contains(korean, `id="direct-audit-title">`+i18n.T(i18n.Korean, "title.audit")+`</h2>`) || strings.Contains(korean, "자동 등록 감사") || strings.Contains(korean, "표시할 자동 등록 이력이 없습니다.") {
		t.Error("old audit title remains")
	}
	s.data.ScopeIssuanceReady = false
	off := owner.call("GET", "/projects/audit?project=demo", nil).Body.String()
	if !strings.Contains(off, i18n.T(i18n.Korean, "audit.auto_off")) {
		t.Error("audit enable gate changed")
	}
}
