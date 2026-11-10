package controlpanel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

type activityView struct {
	State      string `json:"state"`
	Label      string `json:"label"`
	Source     string `json:"source,omitempty"`
	Reason     string `json:"reason,omitempty"`
	ObservedAt string `json:"observedAt,omitempty"`
	ChangedAt  string `json:"changedAt,omitempty"`
	ReceivedAt string `json:"receivedAt,omitempty"`
	ExpiresAt  string `json:"expiresAt,omitempty"`
}

func activityLabel(state string) string {
	switch state {
	case "working":
		return "턴 진행"
	case "idle":
		return "대기"
	case "none":
		return "—"
	default:
		return "활동 확인 불가"
	}
}
func activityTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
func (s Session) ActivityView() activityView {
	v := activityView{State: "unknown", Label: activityLabel("unknown"), Reason: "활동 근거 없음"}
	if s.GrantID != "" {
		return activityView{State: "none", Label: "—"}
	}
	if s.DisplayState() != "ready" || s.Status == "missing" || s.Status == "suspended" || s.Status == "degraded" || s.ErrorCode == "ingress-failed" || s.ErrorCode == "identity-mismatch" {
		v.Reason = "현재 연결의 활동을 확인할 수 없음"
		return v
	}
	a := s.Activity
	if a == nil {
		return v
	}
	switch a.Source {
	case "claude-session-file":
		v.Source = "Claude 세션 상태"
	case "codex-rollout":
		v.Source = "Codex 턴 기록"
	}
	reasons := map[string]string{"unsupported": "지원하지 않는 관측 환경", "evidence-missing": "활동 근거 없음", "identity-mismatch": "세션 식별 불일치", "malformed-evidence": "활동 근거 형식 확인 불가", "stale-evidence": "오래된 활동 근거", "budget-exceeded": "관측 시간 또는 크기 제한", "lease-inactive": "현재 연결의 활동을 확인할 수 없음", "observer-unavailable": "관측기 사용 불가", "observation-expired": "활동 관측 만료"}
	v.Reason = reasons[a.Reason]
	v.ObservedAt, v.ChangedAt, v.ReceivedAt, v.ExpiresAt = activityTime(a.ObservedAt), activityTime(a.ChangedAt), activityTime(a.ReceivedAt), activityTime(a.ExpiresAt)
	if !a.ExpiresAt.IsZero() && time.Now().Before(a.ExpiresAt) && !a.ReceivedAt.IsZero() && (a.State == "working" || a.State == "idle") {
		v.State = string(a.State)
	}
	if !a.ExpiresAt.IsZero() && !time.Now().Before(a.ExpiresAt) {
		v.Reason = "활동 관측 만료"
	}
	v.Label = activityLabel(v.State)
	return v
}

func (s Session) RowKey() string {
	if s.GrantID != "" {
		return "grant:" + s.GrantID
	}
	if s.RegistrationID != "" {
		return "registration:" + s.RegistrationID
	}
	sum := sha256.Sum256([]byte(s.Harness + "\x00" + s.ID + "\x00" + s.ClientID))
	return "legacy:" + hex.EncodeToString(sum[:])
}

// A shape change needs a full reload: it must never silently replace identity,
// ownership or management controls under an open confirmation dialog.
func (s Session) RowShape(canManage bool, owner string) string {
	raw, _ := json.Marshal([]any{s.Name, s.Harness, s.ID, s.SessionID, s.ClientID, s.PID, owner, canManage, s.Suspended, s.ErrorCode == "detached", s.GrantID != ""})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (s Session) ConnectionLabel() string {
	switch s.DisplayState() {
	case "unregistered":
		return "등록 없음"
	case "ready":
		return "수신 준비"
	case "suspended":
		return "중지"
	case "detached":
		return "해제됨"
	case "waiting":
		return "대기"
	default:
		return "확인 불가"
	}
}
func (s Session) ActivityTooltip() []string {
	v := s.ActivityView()
	if v.State == "none" {
		return []string{"활동: — · 등록 없음"}
	}
	lines := []string{"활동: " + v.Label + " · 연결 상태와 다름"}
	for _, item := range []struct{ label, value string }{{"근거", v.Source}, {"사유", v.Reason}} {
		if item.value != "" {
			lines = append(lines, item.label+": "+item.value)
		}
	}
	return lines
}

type activityTooltipTime struct{ Label, Kind, Value, Fallback string }

func (s Session) ActivityTooltipTimes() []activityTooltipTime {
	v := s.ActivityView()
	if v.State == "none" {
		return nil
	}
	var times []activityTooltipTime
	for _, item := range []activityTooltipTime{{Label: "관측 시각", Kind: "observedAt", Value: v.ObservedAt}, {Label: "변경 시각", Kind: "changedAt", Value: v.ChangedAt}, {Label: "서버 수신", Kind: "receivedAt", Value: v.ReceivedAt}, {Label: "신선도 만료", Kind: "expiresAt", Value: v.ExpiresAt}} {
		if value, err := time.Parse(time.RFC3339Nano, item.Value); err == nil {
			item.Fallback = value.UTC().Format("2006-01-02 15:04:05 UTC")
			times = append(times, item)
		}
	}
	return times
}
