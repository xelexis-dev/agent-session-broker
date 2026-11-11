package controlpanel

import (
	"net/url"
	"slices"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

type scopeChoice struct {
	Code, Label, Help string
	Checked, Required bool
}

func projectScopeChoices(form url.Values) []scopeChoice {
	selected := access.LegacyV1Scopes()
	if form.Get("scope_selection") == "custom" {
		selected = form["scope"]
	}
	choices := make([]scopeChoice, 0, 9)
	for _, scope := range access.LegacyV1Scopes() {
		choice := scopeChoice{Code: scope, Label: scopeLabelKey(scope), Checked: slices.Contains(selected, scope), Required: scope == access.SessionRegister}
		if scope == access.SessionManage {
			choice.Help = "tokens.scope_manage_help"
		}
		choices = append(choices, choice)
	}
	return choices
}

func (v tokenView) Auto() bool { return access.HasScope(v.Scopes, access.SessionRegisterAuto) }
func (v tokenView) AdmissionLabel() string {
	if v.Auto() {
		return "tokens.scope_session_register_auto"
	}
	return "tokens.manual_approval"
}

// AutoRegistrationScope retains the machine scope; the template translates the panel label key.
func (p page) AutoRegistrationScope() access.ScopeDetail {
	detail := access.ScopeDetails([]string{access.SessionRegisterAuto})[0]
	detail.Label = scopeLabelKey(access.SessionRegisterAuto)
	return detail
}

func (p page) AutoRegistrationHelp() string {
	return "tokens.auto_registration_help"
}

func scopeLabelKey(code string) string {
	return map[string]string{
		"memory:read":           "tokens.scope_memory_read",
		"memory:write":          "tokens.scope_memory_write",
		"message:receive":       "tokens.scope_message_receive",
		"message:send":          "tokens.scope_message_send",
		"session:manage":        "tokens.scope_session_manage",
		"session:read":          "tokens.scope_session_read",
		"session:register":      "tokens.scope_session_register",
		"session:register:auto": "tokens.scope_session_register_auto",
		"session:rename":        "tokens.scope_session_rename",
	}[code]
}
