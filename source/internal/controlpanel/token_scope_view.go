package controlpanel

import (
	"net/url"
	"slices"
	"strings"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

type scopeChoice struct {
	Code, Label, Help string
	Checked, Required bool
}

func projectScopeChoices(form url.Values) []scopeChoice {
	labels := map[string]string{}
	for _, detail := range access.ScopeCatalog() {
		labels[detail.Scope] = detail.Label
	}
	selected := access.LegacyV1Scopes()
	if form.Get("scope_selection") == "custom" {
		selected = form["scope"]
	}
	choices := make([]scopeChoice, 0, 9)
	for _, scope := range access.LegacyV1Scopes() {
		choice := scopeChoice{Code: scope, Label: labels[scope], Checked: slices.Contains(selected, scope), Required: scope == access.SessionRegister}
		if scope == access.SessionManage {
			choice.Help = "세션 자신은 연결 해제만 할 수 있고, 다른 세션은 연결 해제·일시 중지·재개할 수 있습니다. Owner 토큰은 프로젝트의 모든 세션을, Developer 토큰은 같은 소유자의 세션만 관리합니다."
		}
		choices = append(choices, choice)
	}
	return choices
}

func (v tokenView) Auto() bool { return access.HasScope(v.Scopes, access.SessionRegisterAuto) }
func (v tokenView) AdmissionLabel() string {
	if v.Auto() {
		return (page{}).AutoRegistrationScope().Label
	}
	return "수동 승인"
}

// AutoRegistrationScope keeps the special issuance choice and token list tied
// to the same catalog as credential diagnostics and the README.
func (page) AutoRegistrationScope() access.ScopeDetail {
	return access.ScopeDetails([]string{access.SessionRegisterAuto})[0]
}

// AutoRegistrationHelp shows the first two catalog sentences on the form.
// Credential diagnostics and the README keep the full description.
func (p page) AutoRegistrationHelp() string {
	sentences := strings.SplitAfterN(p.AutoRegistrationScope().Description, ". ", 3)
	return strings.TrimSpace(strings.Join(sentences[:min(2, len(sentences))], ""))
}
