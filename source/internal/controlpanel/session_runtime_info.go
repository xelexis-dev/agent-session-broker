package controlpanel

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"time"
)

type runtimeInfoView struct {
	Model     string `json:"model"`
	Effort    string `json:"effort"`
	Label     string `json:"label"`
	ExpiresAt string `json:"expiresAt"`
}

func (s Session) RuntimeInfoView() runtimeInfoView {
	v := runtimeInfoView{Label: "모델·effort 알 수 없음"}
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
			model = "알 수 없음"
		}
		if effort == "" {
			effort = "알 수 없음"
		}
		v.Label = model + " / " + effort
	}
	return v
}
