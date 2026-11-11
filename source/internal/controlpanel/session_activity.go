package controlpanel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
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

func activityLabel(state string) i18n.Message {
	switch state {
	case "working":
		return i18n.M("session.activity.working")
	case "idle":
		return i18n.M("session.activity.idle")
	case "none":
		return i18n.M("session.activity.none")
	default:
		return i18n.M("session.activity.unknown")
	}
}

var activityReasons = map[string]string{"unsupported": "session.activity.reason.unsupported", "evidence-missing": "session.activity.reason.evidence-missing", "identity-mismatch": "session.activity.reason.identity-mismatch", "malformed-evidence": "session.activity.reason.malformed-evidence", "stale-evidence": "session.activity.reason.stale-evidence", "budget-exceeded": "session.activity.reason.budget-exceeded", "lease-inactive": "session.activity.reason.lease-inactive", "observer-unavailable": "session.activity.reason.observer-unavailable", "observation-expired": "session.activity.reason.observation-expired"}

func activityTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

// ActivityView is the activity in the page's language; the API passes English.
func (s Session) ActivityView(language string) activityView {
	v := activityView{State: "unknown", Label: activityLabel("unknown").In(language), Reason: i18n.T(language, "session.activity.reason.evidence-missing")}
	if s.GrantID != "" {
		return activityView{State: "none", Label: activityLabel("none").In(language)}
	}
	if s.DisplayState() != "ready" || s.Status == "missing" || s.Status == "suspended" || s.Status == "degraded" || s.ErrorCode == "ingress-failed" || s.ErrorCode == "identity-mismatch" {
		v.Reason = i18n.T(language, "session.activity.reason.lease-inactive")
		return v
	}
	a := s.Activity
	if a == nil {
		return v
	}
	switch a.Source {
	case "claude-session-file":
		v.Source = i18n.T(language, "session.activity.source.claude")
	case "codex-rollout":
		v.Source = i18n.T(language, "session.activity.source.codex")
	}
	v.Reason = ""
	if key, ok := activityReasons[a.Reason]; ok {
		v.Reason = i18n.T(language, key)
	}
	v.ObservedAt, v.ChangedAt, v.ReceivedAt, v.ExpiresAt = activityTime(a.ObservedAt), activityTime(a.ChangedAt), activityTime(a.ReceivedAt), activityTime(a.ExpiresAt)
	if !a.ExpiresAt.IsZero() && time.Now().Before(a.ExpiresAt) && !a.ReceivedAt.IsZero() && (a.State == "working" || a.State == "idle") {
		v.State = string(a.State)
	}
	if !a.ExpiresAt.IsZero() && !time.Now().Before(a.ExpiresAt) {
		v.Reason = i18n.T(language, "session.activity.reason.observation-expired")
	}
	v.Label = activityLabel(v.State).In(language)
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
func (s Session) ConnectionLabel() i18n.Message {
	switch s.DisplayState() {
	case "unregistered":
		return i18n.M("session.connection.unregistered")
	case "ready":
		return i18n.M("session.connection.ready")
	case "suspended":
		return i18n.M("session.connection.suspended")
	case "detached":
		return i18n.M("session.connection.detached")
	case "waiting":
		return i18n.M("session.connection.waiting")
	default:
		return i18n.M("session.connection.unreachable")
	}
}
func (s Session) ActivityTooltip(language string) []string {
	v := s.ActivityView(language)
	if v.State == "none" {
		return []string{i18n.T(language, "session.tooltip.activity_none")}
	}
	lines := []string{i18n.T(language, "session.tooltip.activity", "label", v.Label)}
	if v.Source != "" {
		lines = append(lines, i18n.T(language, "session.tooltip.source", "value", v.Source))
	}
	if v.Reason != "" {
		lines = append(lines, i18n.T(language, "session.tooltip.reason", "value", v.Reason))
	}
	return lines
}

type activityTooltipTime struct{ Label, Kind, Value, Fallback string }

func (s Session) ActivityTooltipTimes(language string) []activityTooltipTime {
	v := s.ActivityView(language)
	if v.State == "none" {
		return nil
	}
	var times []activityTooltipTime
	for _, item := range []activityTooltipTime{{Label: i18n.T(language, "session.tooltip.observed_at"), Kind: "observedAt", Value: v.ObservedAt}, {Label: i18n.T(language, "session.tooltip.changed_at"), Kind: "changedAt", Value: v.ChangedAt}, {Label: i18n.T(language, "session.tooltip.received_at"), Kind: "receivedAt", Value: v.ReceivedAt}, {Label: i18n.T(language, "session.tooltip.expires_at"), Kind: "expiresAt", Value: v.ExpiresAt}} {
		if value, err := time.Parse(time.RFC3339Nano, item.Value); err == nil {
			item.Fallback = value.UTC().Format("2006-01-02 15:04:05 UTC")
			times = append(times, item)
		}
	}
	return times
}
