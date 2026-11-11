package controlpanel

import (
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type runtimeInfoView struct {
	Model     string `json:"model"`
	Effort    string `json:"effort"`
	Label     string `json:"label"`
	ExpiresAt string `json:"expiresAt"`
}

func (s Session) RuntimeInfoView(language string) runtimeInfoView {
	v := runtimeInfoView{Label: i18n.T(language, "session.runtime.unknown")}
	if s.GrantID != "" {
		v.Label = "—"
		return v
	}
	info := s.RuntimeInfo
	if info == nil || s.Suspended || s.Status != "alive" || !time.Now().Before(info.ExpiresAt) {
		return v
	}
	if protocol.ValidRuntimeLabel(info.Model, 96) {
		v.Model = info.Model
	}
	if protocol.ValidRuntimeLabel(info.Effort, 16) {
		v.Effort = info.Effort
	}
	v.ExpiresAt = activityTime(info.ExpiresAt)
	if v.Model != "" || v.Effort != "" {
		model, effort := v.Model, v.Effort
		if model == "" {
			model = i18n.T(language, "session.runtime.unknown_part")
		}
		if effort == "" {
			effort = i18n.T(language, "session.runtime.unknown_part")
		}
		v.Label = model + " / " + effort
	}
	return v
}
